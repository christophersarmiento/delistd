package email

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/drumandbytes/delistd/internal/config"
	"github.com/drumandbytes/delistd/internal/oauth"
)

// SMTPSender sends mail through the configured account.
type SMTPSender struct {
	config config.SMTPConfig
	from   string
}

func NewSMTPSender(cfg config.SMTPConfig, from string) *SMTPSender {
	return &SMTPSender{config: cfg, from: from}
}

// Send delivers msg and reports the Message-ID it went out under.
func (s *SMTPSender) Send(ctx context.Context, msg Message) Result {
	fail := func(err error) Result { return Result{Success: false, Error: err} }
	if err := validateMessage(msg); err != nil {
		return fail(err)
	}
	if strings.ContainsAny(msg.Subject, "\r\n") { // header injection
		return fail(fmt.Errorf("subject contains invalid characters"))
	}

	auth, err := s.authenticator(ctx)
	if err != nil {
		return fail(err)
	}
	if auth == nil && s.config.Username != "" {
		return fail(fmt.Errorf("SMTP auth requires TLS"))
	}

	// our own Message-ID, so the one recorded in history is the one the
	// broker receives and quotes back in In-Reply-To
	domain := "localhost"
	if from, err := mail.ParseAddress(msg.From); err == nil {
		domain = from.Address[strings.LastIndex(from.Address, "@")+1:]
	}
	messageID := "<" + rand.Text() + "@" + domain + ">"

	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
	raw := []byte(formatMessage(msg, messageID, time.Now()))
	if err := s.send(ctx, addr, auth, msg.From, msg.To, raw, s.config.TLS()); err != nil {
		return fail(sanitizeSMTPError(err))
	}
	return Result{Success: true, MessageID: messageID}
}

// authenticator is the login for the configured account: a password or an
// OAuth token. It is nil when the connection is not TLS, since credentials
// must never cross in plaintext.
func (s *SMTPSender) authenticator(ctx context.Context) (smtp.Auth, error) {
	if !s.config.TLS() {
		return nil, nil
	}
	secret, err := config.Reveal(s.config.Password)
	if err != nil {
		return nil, err
	}
	if s.config.Auth != config.AuthOAuth2 {
		return smtp.PlainAuth("", s.config.Username, secret, s.config.Host), nil
	}
	token, err := oauth.AccessFor(ctx, config.OAuthClientID(s.config.ClientID), secret)
	if err != nil {
		return nil, err
	}
	return oauth.SMTPAuth(s.config.Username, token), nil
}

// formatMessage builds the RFC 5322 message. A non-ASCII subject is encoded
// as an RFC 2047 word, and a body that is not plain 7-bit text (accented
// names, very long lines) is sent quoted-printable, so no server on the way
// has to support 8-bit mail. Date is required by the RFC and its absence
// counts against a message in spam scoring.
func formatMessage(msg Message, messageID string, now time.Time) string {
	var b strings.Builder
	header := func(name, value string) { b.WriteString(name + ": " + value + "\r\n") }
	header("Message-ID", messageID)
	header("Date", now.Format(time.RFC1123Z))
	header("From", msg.From)
	header("To", msg.To)
	header("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	if plainEnough(msg.Body) {
		header("Content-Transfer-Encoding", "7bit")
		b.WriteString("\r\n" + msg.Body)
		return b.String()
	}
	header("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(msg.Body))
	_ = w.Close()
	return b.String()
}

// plainEnough reports whether text can go out as 7bit: ASCII only, lines no
// longer than the 998 octets RFC 5322 allows.
func plainEnough(text string) bool {
	line := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 0x80 || c == 0 {
			return false
		}
		if c == '\n' {
			line = 0
			continue
		}
		if line++; line > 998 {
			return false
		}
	}
	return true
}

func sanitizeSMTPError(err error) error {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "auth") {
		return fmt.Errorf("SMTP authentication failed")
	}
	if strings.Contains(s, "certificate") {
		return fmt.Errorf("TLS certificate error")
	}
	// "actively refused" is how Windows words the same error.
	if strings.Contains(s, "connection refused") || strings.Contains(s, "actively refused") {
		return fmt.Errorf("could not connect to the SMTP server (wrong host/port, or a local bridge like Proton Mail Bridge isn't running)")
	}
	if strings.Contains(s, "starttls") {
		return fmt.Errorf("SMTP server does not support STARTTLS on this port")
	}
	return fmt.Errorf("SMTP error: check your configuration")
}

// connect dials and secures the connection (TLS from the first byte on 465,
// else mandatory STARTTLS). net/smtp has no context support, so a silent
// server would hang forever and ignore timeouts and Cancel; closing the conn
// on ctx.Done unblocks its reads and writes. stop must be called when done.
func (s *SMTPSender) connect(ctx context.Context, addr string, useTLS bool) (client *smtp.Client, stop func(), err error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connection failed: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close() // unblocks any in-flight Read/Write below
		case <-done:
		}
	}()
	stop = func() { close(done) }

	// 465 is TLS from the first byte; any other port upgrades with STARTTLS.
	implicit := useTLS && config.ImplicitTLS(s.config.Port)
	smtpConn := conn
	if implicit {
		tlsConn := tls.Client(conn, config.TLSFor(s.config.Host))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			stop()
			return nil, nil, fmt.Errorf("TLS handshake failed: %w", err)
		}
		smtpConn = tlsConn
	}

	client, err = smtp.NewClient(smtpConn, s.config.Host)
	if err != nil {
		_ = smtpConn.Close()
		stop()
		return nil, nil, fmt.Errorf("SMTP client creation failed: %w", err)
	}

	if useTLS && !implicit {
		// Required, never opportunistic: auth must not go out in plaintext.
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			stop()
			return nil, nil, fmt.Errorf("server does not offer STARTTLS on port %d", s.config.Port)
		}
		if err := client.StartTLS(config.TLSFor(s.config.Host)); err != nil {
			_ = client.Close()
			stop()
			return nil, nil, fmt.Errorf("STARTTLS failed: %w", err)
		}
	}
	return client, stop, nil
}

// VerifyLogin connects and authenticates without sending anything, so setup
// can tell a bad login from a working one. The error keeps the server's own
// words (unlike Send's sanitized one) because the caller shows it to the
// person who just set the account up.
func (s *SMTPSender) VerifyLogin(ctx context.Context) error {
	if !s.config.TLS() {
		return fmt.Errorf("SMTP login needs TLS")
	}
	auth, err := s.authenticator(ctx)
	if err != nil {
		return err
	}
	client, stop, err := s.connect(ctx, fmt.Sprintf("%s:%d", s.config.Host, s.config.Port), true)
	if err != nil {
		return err
	}
	defer stop()
	defer func() { _ = client.Close() }()
	if err := client.Auth(auth); err != nil {
		return err
	}
	return client.Quit()
}

// send runs the SMTP transaction under ctx.
func (s *SMTPSender) send(ctx context.Context, addr string, auth smtp.Auth, from, to string, msg []byte, useTLS bool) error {
	client, stop, err := s.connect(ctx, addr, useTLS)
	if err != nil {
		return err
	}
	defer stop()
	defer func() { _ = client.Close() }()

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("sender rejected: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("recipient rejected: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data command failed: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("message write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message finalization failed: %w", err)
	}
	return client.Quit()
}
