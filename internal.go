package email

import (
	"crypto/tls"
	"fmt"
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
	// Use a dialer with timeout for robustness
	conn, err := tls.DialWithDialer(dialerFactory(smtpDialTimeout), "tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	return sendWithClient(conn, host, auth, from, to, msg, true)
}

func tryStartTLS(host, addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	conn, err := dialerFactory(smtpDialTimeout).Dial("tcp", addr)
	if err != nil {
		return err
	}
	return sendWithClient(conn, host, auth, from, to, msg, false)
}

func sendWithClient(conn net.Conn, host string, auth smtp.Auth, from string, to []string, msg []byte, alreadyTLS bool) error {
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()

	hostname := resolveHostname()
	// Issue EHLO/Hello to ensure extensions are populated prior to checking STARTTLS support
	if err := client.Hello(hostname); err != nil {
		return err
	}

	if !alreadyTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp server does not support STARTTLS; TLS required")
		}
		tlsCfg := &tls.Config{ServerName: host}
		if err := client.StartTLS(tlsCfg); err != nil {
			return err
		}
		// Re-issue EHLO/Hello after STARTTLS per RFC 3207
		if err := client.Hello(hostname); err != nil {
			return err
		}
	}

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}

	if err := client.Mail(from); err != nil {
		return err
	}
	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return err
		}
	}

	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		wc.Close()
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}

	if err := client.Quit(); err != nil {
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
