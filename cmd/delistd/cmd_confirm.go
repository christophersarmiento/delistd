package main

import (
	"fmt"
	"time"

	"github.com/drumandbytes/delistd/internal/broker"
	"github.com/drumandbytes/delistd/internal/browser"
	"github.com/drumandbytes/delistd/internal/config"
	"github.com/drumandbytes/delistd/internal/history"
	"github.com/spf13/cobra"
)

func confirmCmd() *cobra.Command {
	var confirmURL string
	var brokerID string
	var pending bool
	var validateDomain bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "confirm",
		Short: "Click confirmation links from broker emails",
		Long: `Automatically click confirmation links received from data brokers.

This command makes HTTP GET requests to confirmation URLs to complete the opt-out process.
It follows redirects and verifies success based on the response content.

Examples:
  # Confirm a specific URL
  delistd confirm --url "https://broker.com/confirm?token=abc123"

  # Confirm for a specific broker (using URL from pipeline)
  delistd confirm --broker spokeo

  # Confirm all pending confirmation links
  delistd confirm --pending

  # Preview without actually clicking (dry run)
  delistd confirm --pending --dry-run

Safety features:
  - Domain validation ensures links are from known broker domains
  - Follows redirects up to 10 hops
  - Detects success/failure from response content`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfirm(confirmURL, brokerID, pending, validateDomain, dryRun)
		},
	}

	cmd.Flags().StringVar(&confirmURL, "url", "", "Direct confirmation URL to click")
	cmd.Flags().StringVar(&brokerID, "broker", "", "Broker ID to confirm for (uses URL from pipeline)")
	cmd.Flags().BoolVar(&pending, "pending", false, "Confirm all pending confirmation links")
	cmd.Flags().BoolVar(&validateDomain, "validate-domain", true, "Validate URL domain against known brokers (private/localhost addresses are always blocked regardless of this flag)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview links without clicking them")

	return cmd
}

func runConfirm(confirmURL, brokerID string, pending, validateDomain, dryRun bool) error {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	activeProfile, err := resolveProfile(cfg)
	if err != nil {
		return err
	}
	brokerDB, err := broker.Load(brokerFile)
	if err != nil {
		return fmt.Errorf("failed to load brokers: %w", err)
	}
	store, err := openHistoryStore(history.DBPathFor(resolveConfigPath()))
	if err != nil {
		return fmt.Errorf("failed to initialize history: %w", err)
	}
	defer func() { _ = store.Close() }()

	var brokerDomains []string
	for _, b := range brokerDB.Brokers {
		brokerDomains = append(brokerDomains, b.Domains()...)
	}
	handler := browser.NewConfirmationHandler(brokerDomains)

	fmt.Println("🔗 Confirmation Link Handler")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	links, err := storedLinksFor(store, activeProfile.ID, "confirmation_required", confirmURL, brokerID, pending,
		func(r history.BrokerResponse) string { return r.ConfirmURL }, "no confirmation URL found for broker")
	if err != nil {
		return err
	}
	if len(links) == 0 {
		fmt.Println("✅ No pending confirmation links")
		return nil
	}

	fmt.Printf("📋 Confirmation links to process: %d\n", len(links))
	if dryRun {
		fmt.Println("🔍 DRY RUN MODE - Links will not be clicked")
	}
	fmt.Println()

	confirmed, failed := 0, 0
	for i, link := range links {
		fmt.Printf("[%d/%d] Processing confirmation link\n", i+1, len(links))
		if link.BrokerID != "" {
			fmt.Printf("       Broker: %s\n", link.BrokerID)
		}
		fmt.Printf("       URL: %s\n", truncateURL(link.URL, 60))

		switch clickOne(handler, store, activeProfile.ID, link, validateDomain, dryRun) {
		case clickOK:
			confirmed++
		case clickFailed:
			failed++
		}
		fmt.Println()
		if i < len(links)-1 && !dryRun {
			time.Sleep(time.Second) // be gentle with the broker
		}
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	if dryRun {
		fmt.Printf("📊 Dry run complete: %d links would be clicked\n", len(links))
	} else {
		fmt.Printf("📊 Complete: %d confirmed, %d failed\n", confirmed, failed)
	}
	return nil
}

type clickOutcome int

const (
	clickOK clickOutcome = iota
	clickFailed
)

// clickOne checks, clicks and reports one link, and records the outcome in the
// pipeline.
func clickOne(handler *browser.ConfirmationHandler, store *history.Store, profileID string, link storedLink, validateDomain, dryRun bool) clickOutcome {
	if validateDomain {
		valid, domain, err := handler.ValidateDomain(link.URL)
		if err != nil {
			fmt.Printf("       ❌ Invalid URL: %v\n", err)
			return clickFailed
		}
		if !valid {
			fmt.Printf("       ⚠️  Domain %s is not a known broker domain\n", domain)
			fmt.Printf("       Use --validate-domain=false to override\n")
			return clickFailed
		}
		fmt.Printf("       ✓ Domain validated: %s\n", domain)
	}
	if dryRun {
		fmt.Printf("       📋 Would click this link (dry run)\n")
		return clickOK
	}

	result, err := handler.ClickConfirmationLink(link.URL, false) // the domain was checked just above
	if err != nil {
		fmt.Printf("       ❌ Error: %v\n", err)
		return clickFailed
	}
	fmt.Printf("       HTTP Status: %d\n", result.StatusCode)
	if len(result.RedirectPath) > 1 {
		fmt.Printf("       Redirects: %d hops\n", len(result.RedirectPath)-1)
	}
	if result.FinalURL != link.URL {
		fmt.Printf("       Final URL: %s\n", truncateURL(result.FinalURL, 60))
	}

	status := handler.ExtractConfirmationStatus(result)
	stage, outcome, mark := history.PipelineConfirmed, clickOK, "✅"
	if !result.Success {
		stage, outcome, mark = history.PipelineFailed, clickFailed, "⚠️ "
	}
	fmt.Printf("       %s %s\n", mark, status)
	if link.BrokerID != "" {
		_ = store.UpdatePipelineStatus(profileID, link.BrokerID, stage)
	}
	return outcome
}

// truncateURL shortens a URL for display to maxLen characters, ending in "...".
func truncateURL(url string, maxLen int) string {
	r := []rune(url)
	if len(r) <= maxLen {
		return url
	}
	return string(r[:maxLen-3]) + "..."
}
