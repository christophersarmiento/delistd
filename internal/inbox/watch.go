package inbox

import (
	"context"
	"fmt"
	"log"

	"github.com/emersion/go-imap/client"
)

// WatchForNewEmails blocks, calling callback for each new broker email, until
// ctx ends or the connection fails. It waits in IMAP IDLE and fetches the last
// day's mail whenever the server reports a change.
func (m *Monitor) WatchForNewEmails(ctx context.Context, callback func(Email)) error {
	if m.client == nil {
		return errNotConnected
	}
	if _, err := m.client.Select(m.config.Folder, false); err != nil {
		return fmt.Errorf("failed to select mailbox: %w", err)
	}

	// The client blocks on its Updates channel until it is read, and the
	// fetch below SELECTs again, which emits EXISTS into it; so something
	// always reads it. That goroutine lives as long as the connection.
	updates := make(chan client.Update, 16)
	newMail := make(chan struct{}, 1)
	go func() {
		for u := range updates {
			if _, ok := u.(*client.MailboxUpdate); ok {
				select {
				case newMail <- struct{}{}:
				default: // one pending wake-up is enough
				}
			}
		}
	}()
	m.client.Updates = updates

	// idle runs IDLE until stop is closed; the result arrives on idleDone
	idleDone := make(chan error, 1)
	idle := func() chan struct{} {
		stop := make(chan struct{})
		go func() { idleDone <- m.client.Idle(stop, nil) }()
		return stop
	}
	stop := idle()
	log.Printf("Watching for new emails (press Ctrl+C to stop)...")

	for {
		select {
		case <-ctx.Done():
			// wait for the IDLE goroutine to end, so the caller never uses
			// the connection while it is still in use here
			close(stop)
			_ = <-idleDone // the stop error is not interesting
			return ctx.Err()
		case <-newMail:
			log.Printf("New mail detected")
			close(stop)
			_ = <-idleDone // the stop error is not interesting

			emails, err := m.FetchBrokerEmails(ctx, 1)
			if err != nil {
				log.Printf("Error fetching new email: %v", err)
			}
			for _, e := range emails {
				callback(e)
			}
			select { // the fetch's own SELECT reported the mail again
			case <-newMail:
			default:
			}
			stop = idle()
		case err := <-idleDone:
			if err != nil {
				return fmt.Errorf("IDLE error: %w", err)
			}
		}
	}
}
