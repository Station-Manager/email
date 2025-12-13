package email

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Station-Manager/adif"
	"github.com/Station-Manager/config"
	"github.com/Station-Manager/errors"
	"github.com/Station-Manager/logging"
	"github.com/Station-Manager/types"
)

const ServiceName = types.EmailServiceName

// sendMailFn is a package-level indirection to smtp.SendMail to enable testing without network.
var sendMailFn = sendMailWithTLS

type Service struct {
	ConfigService *config.Service  `di.inject:"configservice"`
	LoggerService *logging.Service `di.inject:"loggingservice"`
	Config        *types.EmailConfig

	isInitialized atomic.Bool
	initOnce      sync.Once
}

type MsgDef struct {
	From string
	To   []string
	Msg  string
}

func (s *Service) Initialize() error {
	const op errors.Op = "email.Service.Initialize"
	if s.isInitialized.Load() {
		return nil
	}

	var initErr error
	s.initOnce.Do(func() {
		if s.LoggerService == nil {
			initErr = errors.New(op).Msg("logger service has not been set/injected")
			return
		}

		if s.ConfigService == nil {
			initErr = errors.New(op).Msg("application config has not been set/injected")
			return
		}

		cfg, err := s.ConfigService.EmailConfig()
		if err != nil {
			initErr = errors.New(op).Err(err).Msg("getting email config")
			return
		}
		s.Config = &cfg

		if err = s.validateConfig(op); err != nil {
			initErr = err
			s.Config.Enabled = false
			return
		}

		// Configure SMTP dial timeout from config, with sane bounds
		if cfg.SmtpDialTimeoutSec > 0 {
			d := time.Duration(cfg.SmtpDialTimeoutSec) * time.Second
			if d < time.Second {
				d = time.Second
			}
			if d > 60*time.Second {
				d = 60 * time.Second
			}
			smtpDialTimeout = d
		} else {
			smtpDialTimeout = 10 * time.Second
		}

		s.isInitialized.Store(true)
	})

	return initErr
}

// Send sends an email message using SMTP configuration, with support for retries and error handling.
func (s *Service) Send(email MsgDef) error {
	const op errors.Op = "email.Service.Send"
	if !s.isInitialized.Load() {
		return errors.New(op).Msg(errMsgNotInitialized)
	}
	if !s.Config.Enabled {
		s.LoggerService.WarnWith().Msg("email service is disabled in the config")
		return nil
	}

	host := strings.TrimSpace(s.Config.Host)
	username := strings.TrimSpace(s.Config.Username)
	password := strings.TrimSpace(s.Config.Password)
	from := strings.TrimSpace(email.From)
	if from == "" {
		from = strings.TrimSpace(s.Config.From)
	}
	if from == "" {
		return errors.New(op).Msg("email from address cannot be empty")
	}

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", s.Config.Port))

	var auth smtp.Auth
	if username != "" {
		// Use PLAIN auth when username provided
		auth = smtp.PlainAuth("", username, password, host)
	}

	// Simple retry loop based on config
	retries := s.Config.SmtpRetryCount
	if retries < 0 {
		retries = 0
	}
	delay := time.Duration(s.Config.SmtpRetryDelaySec) * time.Second
	if delay <= 0 {
		delay = 0
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 && delay > 0 {
			time.Sleep(delay)
		}
		if err := sendMailFn(addr, auth, from, email.To, []byte(email.Msg)); err != nil {
			lastErr = err
			s.LoggerService.ErrorWith().Err(err).Str("host", host).Str("addr", addr).Int("attempt", attempt+1).Msg("email send failed")
			continue
		}
		s.LoggerService.InfoWith().Str("host", host).Str("addr", addr).Msg("email sent")
		lastErr = nil
		break
	}
	if lastErr != nil {
		return errors.New(op).Err(lastErr).Msg("failed to send email")
	}

	return nil
}

func (s *Service) BuildEmailWithADIFAttachment(from, subject, msg string, to []string, slice []types.Qso) (MsgDef, error) {
	const op errors.Op = "email.Service.BuildEmailWithADIFAttachment"

	from = strings.TrimSpace(from)
	if from == "" {
		from = s.Config.From
	}
	// Resolve recipients: use a provided list or fallback to config (split by comma/semicolon/space)
	tos := to
	if len(tos) == 0 {
		tos = splitAndTrim(s.Config.To)
	}
	if len(tos) == 0 {
		return MsgDef{}, errors.New(op).Msg("email TO address cannot be empty")
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = s.Config.Subject
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = s.Config.Body
	}
	if len(slice) == 0 {
		return MsgDef{}, errors.New(op).Msg("QSO slice cannot be empty")
	}

	adifContent, err := adif.ComposeToAdifString(slice)
	if err != nil {
		return MsgDef{}, errors.New(op).Err(err).Msg("failed to compose ADIF string")
	}

	filename := fmt.Sprintf("%s-export.adi", time.Now().Format("20060102150405"))
	adifB64 := base64.StdEncoding.EncodeToString([]byte(adifContent))

	// Prepare headers
	hdr := make(textproto.MIMEHeader)
	hdr.Set("From", from)
	hdr.Set("To", strings.Join(tos, ", "))
	hdr.Set("Subject", subject)
	hdr.Set("Date", time.Now().UTC().Format(time.RFC1123Z))
	// Generate a simple message-id
	mid := generateMessageID()
	hdr.Set("Message-ID", mid)
	hdr.Set("MIME-Version", "1.0")

	var buf bytes.Buffer
	// Create a multipart / mixed writer
	mw := multipart.NewWriter(&buf)
	boundary := mw.Boundary()
	hdr.Set("Content-Type", fmt.Sprintf("multipart/mixed; boundary=%q", boundary))

	// Write headers
	for k, v := range hdr {
		if len(v) == 0 {
			continue
		}
		buf.WriteString(k)
		buf.WriteString(": ")
		buf.WriteString(strings.Join(v, ", "))
		buf.WriteString("\r\n")
	}
	buf.WriteString("\r\n")

	// Body part (text/plain; quoted-printable)
	wp, err := mw.CreatePart(mapToMIMEHeader(map[string]string{
		"Content-Type":              "text/plain; charset=utf-8",
		"Content-Transfer-Encoding": "quoted-printable",
	}))
	if err != nil {
		return MsgDef{}, errors.New(op).Err(err).Msg("create body part")
	}

	qp := quotedprintable.NewWriter(wp)
	if _, err = qp.Write([]byte(msg)); err != nil {
		return MsgDef{}, errors.New(op).Err(err).Msg("write body")
	}
	if err = qp.Close(); err != nil {
		return MsgDef{}, errors.New(op).Err(err).Msg("close qp")
	}

	// Attachment part
	ap, err := mw.CreatePart(mapToMIMEHeader(map[string]string{
		"Content-Type":              fmt.Sprintf("application/octet-stream; name=%q", filename),
		"Content-Transfer-Encoding": "base64",
		"Content-Disposition":       fmt.Sprintf("attachment; filename=%q", filename),
	}))
	if err != nil {
		return MsgDef{}, errors.New(op).Err(err).Msg("create attachment part")
	}

	// 76-chunked base64 with CRLF
	for i := 0; i < len(adifB64); i += 76 {
		end := i + 76
		if end > len(adifB64) {
			end = len(adifB64)
		}
		if _, err := ap.Write([]byte(adifB64[i:end])); err != nil {
			return MsgDef{}, errors.New(op).Err(err).Msg("write attachment part")
		}
		if _, err := ap.Write([]byte("\r\n")); err != nil {
			return MsgDef{}, errors.New(op).Err(err).Msg("write attachment newline")
		}
	}
	if err := mw.Close(); err != nil {
		return MsgDef{}, errors.New(op).Err(err).Msg("finalize multipart")
	}

	return MsgDef{From: from, To: tos, Msg: buf.String()}, nil
}
