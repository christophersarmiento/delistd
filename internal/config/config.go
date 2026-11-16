package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/drumandbytes/delistd/internal/country"
	"github.com/drumandbytes/delistd/internal/datadir"
	"github.com/drumandbytes/delistd/internal/fsutil"
	"gopkg.in/yaml.v3"
)

const defaultRateLimitMs = 2000

// defaultDailySendLimit stays safely under Gmail's ~500/day cap for a
// regular (non-Workspace) account, leaving headroom for other mail you send
// that same day.
const defaultDailySendLimit = 450

func checkFilePermissions(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		return fmt.Errorf("config file %s has insecure permissions %04o; should be 0600", path, perm)
	}
	return nil
}

type Config struct {
	// Profiles tracks removal requests for several people against the same
	// brokers, mail account and options; history is kept per profile
	// (profile_id). Takes precedence over Profile.
	Profiles []NamedProfile `yaml:"profiles,omitempty"`
	// PassphraseProtected: mail passwords are encrypted with a passphrase on
	// top of the machine binding (see secret.go). Set with `delistd passphrase set`.
	PassphraseProtected bool        `yaml:"passphrase_protected,omitempty"`
	Email               EmailConfig `yaml:"email"`
	Options             Options     `yaml:"options"`
	Inbox               InboxConfig `yaml:"inbox,omitempty"`
	Pipeline            Pipeline    `yaml:"pipeline,omitempty"`
	Schedule            Schedule    `yaml:"schedule,omitempty"`
}

// Schedule is the in-app fallback for automated cycles when the OS scheduler
// ('delistd schedule install') isn't set up.
type Schedule struct {
	// Enabled makes a running 'delistd serve' run a cycle every 6 hours.
	// Ignored while the OS job is installed.
	Enabled bool `yaml:"enabled,omitempty"`
}

// Pipeline holds settings for the automation pipeline
type Pipeline struct {
	// BrowserHeadless is a pointer so an explicit false survives loading (a
	// plain bool was forced back to true). Read it via Headless().
	BrowserHeadless   *bool `yaml:"browser_headless,omitempty"`
	BrowserTimeoutSec int   `yaml:"browser_timeout_sec"` // Browser operation timeout
}

// Headless returns the effective headless setting, defaulting to true when
// BrowserHeadless was never set in the config.
func (p Pipeline) Headless() bool {
	if p.BrowserHeadless == nil {
		return true
	}
	return *p.BrowserHeadless
}

type Options struct {
	Template string `yaml:"template"`
	// CustomTemplate is the body of the user-written "custom" template
	// (Go text/template, same fields as the built-ins). An optional first
	// line "Subject: ..." sets the subject. Used when Template is "custom".
	CustomTemplate string `yaml:"custom_template,omitempty"`
	// SendMode:
	//   "" / "smtp" - Delistd sends over the configured SMTP account.
	//   "manual"    - Delistd renders the emails for you to send yourself and
	//                 record with `delistd mark-sent` / "Mark sent". No email:
	//                 block needed; for users who won't share mailbox credentials.
	SendMode    string `yaml:"send_mode,omitempty"`
	DryRun      bool   `yaml:"dry_run,omitempty"`
	RateLimitMs int    `yaml:"rate_limit_ms"`
	// DailySendLimit caps sends per rolling 24h to stay under provider limits
	// (Gmail ~500/day). 0 = 450. Bypass with --ignore-daily-limit.
	DailySendLimit int `yaml:"daily_send_limit,omitempty"`
	// BrokerList: "" / "full" (~720 entries, default) or "verified"
	// (data/brokers-verified.yaml). Overridden by `send --list`; ignored when
	// BrokerFile or --brokers is set.
	BrokerList string `yaml:"broker_list,omitempty"`
	// BrokerFile points the send-family commands at your own broker list,
	// the config equivalent of the global --brokers flag (which still wins).
	// Takes precedence over BrokerList.
	BrokerFile      string   `yaml:"broker_file,omitempty"`
	Regions         []string `yaml:"regions,omitempty"`
	ExcludedBrokers []string `yaml:"excluded_brokers,omitempty"`
	// ExcludedCategories skips brokers by category (case-insensitive), e.g.
	// "requires-id" for brokers that demand an ID document.
	ExcludedCategories []string `yaml:"excluded_categories,omitempty"`
}

func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(datadir.Of(home), "config.yaml")
}

func Load(path string) (*Config, error) {
	if err := checkFilePermissions(path); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: %v\n", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if cfg.Options.Template == "" {
		// Fork default is gdpr, not upstream's generic - see EU-NOTES.md.
		// Only fills in a genuinely missing field; an explicit template in
		// config.yaml (which every real user of this fork will have) wins.
		cfg.Options.Template = "gdpr"
	}
	if cfg.Options.RateLimitMs == 0 {
		cfg.Options.RateLimitMs = defaultRateLimitMs
	}
	if cfg.Options.DailySendLimit == 0 {
		cfg.Options.DailySendLimit = defaultDailySendLimit
	}

	plaintext := false
	cfg.eachSecret(func(p *string) {
		if *p != "" && !isEncrypted(*p) {
			plaintext = true
			return
		}
		if strings.HasPrefix(*p, secretPrefixV2) {
			return // decrypted by Reveal, when the passphrase is available
		}
		plain, err := decryptSecret(*p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: %v; re-enter it with `delistd init` or in Settings\n", err)
			plain = ""
		}
		*p = plain
	})
	migrateFrom1x(path, data, &cfg, plaintext)
	applyInboxDefaults(&cfg.Inbox)
	for i := range cfg.Profiles {
		if cfg.Profiles[i].Mail != nil && cfg.Profiles[i].Mail.Inbox != nil {
			applyInboxDefaults(cfg.Profiles[i].Mail.Inbox)
		}
	}

	if cfg.Pipeline.BrowserTimeoutSec == 0 {
		cfg.Pipeline.BrowserTimeoutSec = 30
	}
	// BrowserHeadless is intentionally left as-is here (nil if unset) -
	// see Pipeline.Headless(), which applies the true default without
	// clobbering an explicit false.

	return &cfg, nil
}

// maskPasswords blanks every non-empty password: value in a config document, so
// the 1.x backup never keeps a password in plain text. A document without any
// comes back byte for byte; one it can't parse yields nothing rather than risk
// keeping a password.
func maskPasswords(doc []byte) []byte {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil
	}
	masked := 0
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if v := n.Content[i+1]; n.Content[i].Value == "password" && v.Kind == yaml.ScalarNode && v.Value != "" {
					v.Value, v.Tag = "", "!!str"
					masked++
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	if masked == 0 {
		return doc
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil
	}
	return out
}

// migrateFrom1x upgrades a 1.x config in memory and on disk: the pre-0.11
// top-level profile: block becomes profiles: [{id: default}], and free-text
// profile countries become ISO alpha-2 codes. Before the first rewrite the
// original file is kept as <path>.1x.bak. A country that doesn't resolve is
// left alone and warned about. email.provider needs no step: it is no longer
// read and drops out on the next save.
func migrateFrom1x(path string, orig []byte, cfg *Config, plaintextSecrets bool) {
	changed := plaintextSecrets
	var legacy struct {
		Profile Profile `yaml:"profile"`
	}
	if len(cfg.Profiles) == 0 && yaml.Unmarshal(orig, &legacy) == nil && !reflect.ValueOf(legacy.Profile).IsZero() {
		cfg.Profiles, changed = []NamedProfile{{ID: DefaultProfileID, Profile: legacy.Profile}}, true
	}
	for i := range cfg.Profiles {
		p := &cfg.Profiles[i]
		if p.Country == "" {
			continue
		}
		code, ok := country.Code(p.Country)
		if !ok {
			fmt.Fprintf(os.Stderr, "WARNING: profile %q: country %q isn't a known country; fix it with `delistd profile edit` (country-scoped brokers aren't filtered until then)\n", p.ID, p.Country)
			continue
		}
		if code != p.Country {
			p.Country, changed = code, true
		}
	}
	if !changed {
		return
	}
	bak := path + ".1x.bak"
	if _, err := os.Stat(bak); err != nil {
		masked := maskPasswords(orig)
		if masked == nil {
			fmt.Fprintf(os.Stderr, "WARNING: could not back up config before migrating it from 1.x; leaving it unchanged\n")
			return
		}
		if err := os.WriteFile(bak, masked, 0600); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: could not back up config before migrating it from 1.x: %v\n", err)
			return
		}
	}
	if err := Save(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not save the migrated config: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "Upgraded %s from 1.x (country codes, encrypted passwords). The original is kept as %s with passwords blanked.\n", filepath.Base(path), filepath.Base(bak))
}

func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	out := cfg.cloneForSecrets() // cfg may be the live config others read
	if err := out.secretFields(func(p *string) error {
		if out.PassphraseProtected && *p != "" && !isEncrypted(*p) {
			pass, err := passphrase()
			if err != nil {
				return err
			}
			*p = encryptV2(*p, pass)
			return nil
		}
		*p = encryptSecret(*p)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to protect passwords: %w", err)
	}
	data, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}
	return fsutil.WriteFileAtomic(path, data, 0o600)
}

func (c *Config) Validate() error {
	profiles := c.GetProfiles()
	seen := make(map[string]bool, len(profiles))
	manual := c.IsManualSend()
	for _, np := range profiles {
		if np.ID == "" {
			return fmt.Errorf("profiles: every profile needs an id")
		}
		key := strings.ToLower(np.ID)
		if seen[key] {
			return fmt.Errorf("profiles: duplicate profile id %q", np.ID)
		}
		seen[key] = true
		if np.FirstName == "" || np.LastName == "" {
			return fmt.Errorf("profile %q: first_name and last_name are required", np.ID)
		}
		if np.Email == "" {
			return fmt.Errorf("profile %q: email is required", np.ID)
		}
		if !manual && np.Mail != nil && np.Mail.Email != nil {
			if err := validateEmailConfig(*np.Mail.Email); err != nil {
				return fmt.Errorf("profile %q: mail.email: %w", np.ID, err)
			}
		}
	}

	// Manual mode sends nothing itself, so it needs no email configuration.
	if manual {
		return nil
	}

	return validateEmailConfig(c.Email)
}

// IsManualSend reports whether the user opted out of automated sending -
// Delistd renders the emails but never transmits them.
func (c *Config) IsManualSend() bool {
	return strings.EqualFold(c.Options.SendMode, "manual")
}
