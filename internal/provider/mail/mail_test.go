package mail

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestLogModeOnlyAllowedInDevelopment(t *testing.T) {
	if _, err := NewSender(Config{Environment: "production", Mode: ModeLog}, log.Default()); err == nil {
		t.Fatal("production accepted SMTP_MODE=log")
	}
	var output bytes.Buffer
	sender, err := NewSender(Config{Environment: "development", Mode: ModeLog}, log.New(&output, "", 0))
	if err != nil {
		t.Fatalf("new log sender: %v", err)
	}
	if err := sender.SendRegistrationCode(context.Background(), "alice@example.com", "012345"); err != nil {
		t.Fatalf("send: %v", err)
	}
	logged := output.String()
	if !strings.Contains(logged, "purpose=registration") || !strings.Contains(logged, "to=alice@example.com") {
		t.Fatalf("development log missing non-sensitive delivery metadata: %q", logged)
	}
	for _, secret := range []string{"012345", "code=", "password=", "token=", "auth_code="} {
		if strings.Contains(strings.ToLower(logged), secret) {
			t.Fatalf("log contains forbidden field %q: %q", secret, logged)
		}
	}
}

func TestSMTPModeRequiresCompleteSecureConfig(t *testing.T) {
	if _, err := NewSender(Config{Environment: "production", Mode: ModeSMTP}, nil); err == nil {
		t.Fatal("incomplete SMTP config accepted")
	}
	if _, err := NewSender(Config{
		Environment: "production", Mode: ModeSMTP, From: "bot@qq.com", AuthCode: "authorization-code", TLSMode: "plain",
	}, nil); err == nil {
		t.Fatal("unsupported TLS mode accepted")
	}
	sender, err := NewSender(Config{
		Environment: "production", Mode: ModeSMTP, From: "bot@qq.com", AuthCode: "authorization-code",
	}, nil)
	if err != nil {
		t.Fatalf("QQ implicit TLS defaults rejected: %v", err)
	}
	smtpImpl := sender.(*smtpSender)
	if smtpImpl.cfg.Host != "smtp.qq.com" || smtpImpl.cfg.Port != 465 || smtpImpl.cfg.TLSMode != TLSImplicit {
		t.Fatalf("unexpected QQ defaults: %+v", smtpImpl.cfg)
	}
}

func TestChinesePlainTextTemplatesArePurposeSpecific(t *testing.T) {
	registration := string(formatMessage("bot@qq.com", "alice@example.com", "注册验证码", "验证码 012345"))
	reset := string(formatMessage("bot@qq.com", "alice@example.com", "密码重置验证码", "验证码 654321"))
	for name, message := range map[string]string{"registration": registration, "reset": reset} {
		if !strings.Contains(message, "Content-Type: text/plain; charset=UTF-8") {
			t.Fatalf("%s is not UTF-8 plain text: %q", name, message)
		}
		if !strings.Contains(message, "=?UTF-8?") {
			t.Fatalf("%s subject is not encoded: %q", name, message)
		}
	}
	if registration == reset {
		t.Fatal("registration and reset templates are identical")
	}
}

func TestFakeSenderIsInjectable(t *testing.T) {
	fake := &FakeSender{}
	if err := fake.SendPasswordResetCode(context.Background(), "alice@example.com", "123456"); err != nil {
		t.Fatalf("fake send: %v", err)
	}
	message, ok := fake.Last()
	if !ok || message.Purpose != "password_reset" || message.Code != "123456" {
		t.Fatalf("unexpected fake message: %+v", message)
	}
}
