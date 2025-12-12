package email

import (
	"crypto/tls"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Station-Manager/errors"
)

func (s *Service) validateConfig(op errors.Op) error {
	// Quick sanity check to ensure TLS enforcement has needed inputs
	host := strings.TrimSpace(s.Config.Host)
	if host == "" {
		return errors.New(op).Msg("email host cannot be empty")
	}
	if strings.Contains(host, " ") {
		return errors.New(op).Msg("email host cannot contain spaces")
	}
	if s.Config.Port <= 0 || s.Config.Port > 65535 {
		return errors.New(op).Msg("email port must be between 1 and 65535")
	}
	// Use JoinHostPort to be IPv6-safe during validation
	addr := net.JoinHostPort(host, strconv.Itoa(s.Config.Port))
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return errors.New(op).Err(err).Msg("invalid host or port for email config")
	}
	from := strings.TrimSpace(s.Config.From)
	if from == "" {
		return errors.New(op).Msg("email from address cannot be empty")
	}
	username := strings.TrimSpace(s.Config.Username)
	password := strings.TrimSpace(s.Config.Password)
	if username == "" && password != "" {
		return errors.New(op).Msg("email username must be set when password is provided")
	}
	if password == "" && username != "" {
		return errors.New(op).Msg("email password must be set when username is provided")
	}
	return nil
}

// dialerFactory allows tests to override dialer behavior
var dialerFactory = func(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
}

// smtpDialTimeout controls outbound SMTP dial deadlines; set by service Initialize
var smtpDialTimeout = 10 * time.Second

func sendMailWithTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	const op errors.Op = "email.sendMailWithTLS"
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New(op).Err(err).Msg("invalid smtp address")
	}

	if err = tryImplicitTLS(host, addr, auth, from, to, msg); err == nil {
		return nil
	}

	return tryStartTLS(host, addr, auth, from, to, msg)
}

func tryImplicitTLS(host, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	const op errors.Op = "email.tryImplicitTLS"
	// Use a dialer with timeout for robustness
	conn, err := tls.DialWithDialer(dialerFactory(smtpDialTimeout), "tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return errors.New(op).Err(err)
	}
	return sendWithClient(conn, host, auth, from, to, msg, true)
}

func tryStartTLS(host, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	const op errors.Op = "email.tryStartTLS"
	conn, err := dialerFactory(smtpDialTimeout).Dial("tcp", addr)
	if err != nil {
		return errors.New(op).Err(err)
	}
	return sendWithClient(conn, host, auth, from, to, msg, false)
}

func sendWithClient(conn net.Conn, host string, auth smtp.Auth, from string, to []string, msg []byte, alreadyTLS bool) error {
	const op errors.Op = "email.sendWithClient"
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		cerr := conn.Close()
		if cerr != nil {
			return errors.New(op).Err(cerr)
		}
		return errors.New(op).Err(err)
	}
	defer func(client *smtp.Client) {
		_ = client.Close()
	}(client)

	hostname := resolveHostname()
	// Issue EHLO/Hello to ensure extensions are populated prior to checking STARTTLS support
	if err = client.Hello(hostname); err != nil {
		return errors.New(op).Err(err)
	}

	if !alreadyTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New(op).Msg("smtp server does not support STARTTLS; TLS required")
		}
		tlsCfg := &tls.Config{ServerName: host}
		if cerr := client.StartTLS(tlsCfg); cerr != nil {
			return errors.New(op).Err(cerr)
		}
		// Re-issue EHLO/Hello after STARTTLS per RFC 3207
		if herr := client.Hello(hostname); herr != nil {
			return errors.New(op).Err(herr)
		}
	}

	if auth != nil {
		if aerr := client.Auth(auth); aerr != nil {
			return errors.New(op).Err(aerr)
		}
	}

	if merr := client.Mail(from); merr != nil {
		return merr
	}
	for _, addr := range to {
		if aerr := client.Rcpt(addr); aerr != nil {
			return errors.New(op).Err(aerr)
		}
	}

	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = wc.Write(msg); err != nil {
		cerr := wc.Close()
		if cerr != nil {
			return errors.New(op).Err(cerr)
		}
		return errors.New(op).Err(err)
	}
	if cerr := wc.Close(); cerr != nil {
		return errors.New(op).Err(cerr)
	}

	if qerr := client.Quit(); qerr != nil {
		// message already accepted; treat QUIT failures as best-effort to avoid duplicate retries
		return nil
	}
	return nil
}

func resolveHostname() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "localhost"
	}
	// RFC 5321 recommends letters, digits, and hyphen; replace anything else with '-'
	var b strings.Builder
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		return "localhost"
	}
	return cleaned
}
