package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ModeLog  = "log"
	ModeSMTP = "smtp"

	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
)

// Sender is the delivery seam used by platform service. It deliberately accepts neither
// plaintext passwords nor flow tokens, preventing those secrets from reaching mail transports.
type Sender interface {
	SendRegistrationCode(context.Context, string, string) error
	SendPasswordResetCode(context.Context, string, string) error
}

type Config struct {
	Environment string
	Mode        string
	Host        string
	Port        int
	From        string
	AuthCode    string
	TLSMode     string
	Timeout     time.Duration
}

func NewSender(cfg Config, logger *log.Logger) (Sender, error) {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		if cfg.Environment == "development" {
			cfg.Mode = ModeLog
		} else {
			cfg.Mode = ModeSMTP
		}
	}
	if cfg.Mode == ModeLog {
		if cfg.Environment != "development" {
			return nil, errors.New("SMTP_MODE=log is allowed only in development")
		}
		if logger == nil {
			logger = log.Default()
		}
		return &logSender{logger: logger}, nil
	}
	if cfg.Mode != ModeSMTP {
		return nil, fmt.Errorf("unsupported SMTP_MODE %q", cfg.Mode)
	}
	if cfg.Host == "" {
		cfg.Host = "smtp.qq.com"
	}
	if cfg.Port == 0 {
		cfg.Port = 465
	}
	if cfg.TLSMode == "" {
		cfg.TLSMode = TLSImplicit
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	cfg.TLSMode = strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if cfg.TLSMode != TLSImplicit && cfg.TLSMode != TLSStartTLS {
		return nil, errors.New("SMTP_TLS_MODE must be implicit or starttls")
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.From == "" || cfg.AuthCode == "" {
		return nil, errors.New("SMTP_PORT, SMTP_FROM and SMTP_AUTH_CODE are required in smtp mode")
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, errors.New("SMTP_FROM must be a valid email address")
	}
	cfg.From = from.Address
	return &smtpSender{cfg: cfg}, nil
}

type logSender struct {
	logger *log.Logger
}

func (s *logSender) SendRegistrationCode(_ context.Context, to, _ string) error {
	s.logger.Printf("development verification mail queued purpose=registration to=%s", to)
	return nil
}

func (s *logSender) SendPasswordResetCode(_ context.Context, to, _ string) error {
	s.logger.Printf("development verification mail queued purpose=password_reset to=%s", to)
	return nil
}

type smtpSender struct {
	cfg Config
}

func (s *smtpSender) SendRegistrationCode(ctx context.Context, to, code string) error {
	body := "您好：\r\n\r\n您的注册验证码是：" + code + "\r\n\r\n验证码 10 分钟内有效，请勿向任何人泄露。若非本人操作，请忽略本邮件。\r\n"
	return s.send(ctx, to, "注册验证码", body)
}

func (s *smtpSender) SendPasswordResetCode(ctx context.Context, to, code string) error {
	body := "您好：\r\n\r\n您的密码重置验证码是：" + code + "\r\n\r\n验证码 10 分钟内有效，请勿向任何人泄露。若非本人操作，请忽略本邮件。\r\n"
	return s.send(ctx, to, "密码重置验证码", body)
}

func (s *smtpSender) send(ctx context.Context, to, subject, body string) error {
	if _, err := mail.ParseAddress(to); err != nil || strings.ContainsAny(to, "\r\n") {
		return errors.New("invalid recipient address")
	}
	deadline := time.Now().Add(s.cfg.Timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	dialer := net.Dialer{Timeout: time.Until(deadline)}
	address := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("smtp connection failed: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("smtp deadline failed: %w", err)
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	tlsConfig := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	var client *smtp.Client
	if s.cfg.TLSMode == TLSImplicit {
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp TLS handshake failed: %w", err)
		}
		client, err = smtp.NewClient(tlsConn, s.cfg.Host)
	} else {
		client, err = smtp.NewClient(conn, s.cfg.Host)
		if err == nil {
			err = client.StartTLS(tlsConfig)
		}
	}
	if err != nil {
		return fmt.Errorf("smtp setup failed: %w", err)
	}
	defer client.Close()

	if err := client.Auth(smtp.PlainAuth("", s.cfg.From, s.cfg.AuthCode, s.cfg.Host)); err != nil {
		return errors.New("smtp authentication failed")
	}
	if err := client.Mail(s.cfg.From); err != nil {
		return errors.New("smtp sender rejected")
	}
	if err := client.Rcpt(to); err != nil {
		return errors.New("smtp recipient rejected")
	}
	writer, err := client.Data()
	if err != nil {
		return errors.New("smtp data rejected")
	}
	message := formatMessage(s.cfg.From, to, subject, body)
	if _, err := io.Copy(writer, bytes.NewReader(message)); err != nil {
		_ = writer.Close()
		return errors.New("smtp message write failed")
	}
	if err := writer.Close(); err != nil {
		return errors.New("smtp message completion failed")
	}
	if err := client.Quit(); err != nil {
		return errors.New("smtp quit failed")
	}
	return nil
}

func formatMessage(from, to, subject, body string) []byte {
	encodedSubject := mime.BEncoding.Encode("UTF-8", subject)
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + encodedSubject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" + body)
}

// FakeSender records deliveries and supports deterministic failures for service tests.
type FakeSender struct {
	mu       sync.Mutex
	Messages []Message
	Err      error
}

type Message struct {
	Purpose string
	To      string
	Code    string
}

func (f *FakeSender) SendRegistrationCode(_ context.Context, to, code string) error {
	return f.record(Message{Purpose: "registration", To: to, Code: code})
}

func (f *FakeSender) SendPasswordResetCode(_ context.Context, to, code string) error {
	return f.record(Message{Purpose: "password_reset", To: to, Code: code})
}

func (f *FakeSender) record(message Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.Messages = append(f.Messages, message)
	return nil
}

func (f *FakeSender) Last() (Message, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Messages) == 0 {
		return Message{}, false
	}
	return f.Messages[len(f.Messages)-1], true
}
