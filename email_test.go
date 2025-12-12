package email

import (
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Station-Manager/types"
	"net/smtp"
)

func TestValidateConfig_IPv6AndMessages(t *testing.T) {
	s := &Service{Config: &types.EmailConfig{Host: "2001:db8::1", Port: 587, From: "from@example.com"}}
	if err := s.validateConfig("op"); err != nil {
		t.Fatalf("validateConfig failed for IPv6: %v", err)
	}

	s = &Service{Config: &types.EmailConfig{Host: "", Port: 587, From: "from@example.com"}}
	if err := s.validateConfig("op"); err == nil || !strings.Contains(err.Error(), "email host cannot be empty") {
		t.Fatalf("expected empty host error, got %v", err)
	}

	s = &Service{Config: &types.EmailConfig{Host: "mail", Port: 587, From: ""}}
	if err := s.validateConfig("op"); err == nil || !strings.Contains(err.Error(), "email from address cannot be empty") {
		t.Fatalf("expected empty from error, got %v", err)
	}
}

func TestSend_AddrJoinHostPortAndRetry(t *testing.T) {
	s := &Service{Config: &types.EmailConfig{
		Enabled:           true,
		Host:              "smtp.example.com",
		Port:              587,
		Username:          "user",
		Password:          "pass",
		SmtpRetryCount:    2,
		SmtpRetryDelaySec: 0,
	}}
	s.isInitialized.Store(true)

	var calls int32
	old := sendMailFn
	t.Cleanup(func() { sendMailFn = old })

	sendMailFn = func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
		// signature adapt using type assertion for smtp.Auth is not possible in test, use interface{}/panic if mismatch
		atomic.AddInt32(&calls, 1)
		// ensure address uses JoinHostPort canonical form (host:port)
		if !strings.HasPrefix(addr, "smtp.example.com:") {
			t.Errorf("addr not built with JoinHostPort, got %q", addr)
		}
		if atomic.LoadInt32(&calls) < 3 {
			return assertError("temporary")
		}
		return nil
	}

	email := MsgDef{From: "from@example.com", To: []string{"to@example.com"}, Msg: "hi"}
	if err := s.Send(email); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if c := atomic.LoadInt32(&calls); c != 3 {
		t.Fatalf("expected 3 attempts due to retries, got %d", c)
	}
}

// assertError provides an error implementing Error()
type assertError string

func (e assertError) Error() string { return string(e) }

func TestBuildEmailWithADIFAttachmentHeadersAndBoundary(t *testing.T) {
	s := &Service{Config: &types.EmailConfig{
		From:    "from@example.com",
		To:      "alice@example.com, bob@example.org",
		Subject: "Subject",
		Body:    "Body",
	}}

	// Minimal QSO input
	qs := []types.Qso{{LogbookID: 1, SessionID: 1}}

	msg1, err := s.BuildEmailWithADIFAttachment("", "", "hello world", nil, qs)
	if err != nil {
		t.Fatalf("BuildEmailWithADIFAttachment failed: %v", err)
	}
	if len(msg1.To) != 2 {
		t.Fatalf("expected 2 recipients from config split, got %d", len(msg1.To))
	}
	// Check headers
	if !strings.Contains(msg1.Msg, "Date:") {
		t.Errorf("missing Date header")
	}
	if !strings.Contains(strings.ToLower(msg1.Msg), "\r\nmessage-id: ") {
		t.Errorf("missing Message-ID header")
	}
	// Extract boundary from header Content-Type
	re := regexp.MustCompile(`(?i)Content-Type: multipart/mixed; boundary="([^"]+)"`)
	m := re.FindStringSubmatch(msg1.Msg)
	if len(m) != 2 {
		t.Fatalf("could not find boundary in header")
	}
	boundary := m[1]
	if !strings.Contains(msg1.Msg, "--"+boundary) {
		t.Errorf("boundary marker not found in body")
	}

	// Boundary should be random across calls
	// small sleep to avoid identical timestamps influencing message-id only
	time.Sleep(2 * time.Millisecond)
	msg2, err := s.BuildEmailWithADIFAttachment("", "", "hello world", nil, qs)
	if err != nil {
		t.Fatalf("BuildEmailWithADIFAttachment 2 failed: %v", err)
	}
	m2 := re.FindStringSubmatch(msg2.Msg)
	if len(m2) != 2 {
		t.Fatalf("could not find boundary in header (2)")
	}
	if m2[1] == boundary {
		t.Errorf("expected randomized boundary, got the same value")
	}
}

func TestSplitAndTrim(t *testing.T) {
	inp := "a@x.com; b@y.com c@z.com, d@w.com"
	out := splitAndTrim(inp)
	if len(out) != 4 {
		t.Fatalf("expected 4 items, got %d: %v", len(out), out)
	}
}

func TestSendDefaultsEnvelopeFrom(t *testing.T) {
	s := &Service{Config: &types.EmailConfig{
		Enabled: true,
		Host:    "smtp.example.com",
		Port:    587,
		From:    "cfg@example.com",
	}}
	s.isInitialized.Store(true)

	var capturedFrom string
	old := sendMailFn
	sendMailFn = func(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
		capturedFrom = from
		return nil
	}
	t.Cleanup(func() { sendMailFn = old })

	if err := s.Send(MsgDef{To: []string{"to@example.com"}, Msg: "body"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if capturedFrom != "cfg@example.com" {
		t.Fatalf("expected defaulted from, got %q", capturedFrom)
	}
}
