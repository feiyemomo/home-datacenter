package security

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/config"
)

// ErrAlertNotConfigured is returned when SMTP alerting is disabled or
// missing host / credentials / recipient.
var ErrAlertNotConfigured = errors.New("SMTP alert not configured or disabled")

// AlertManager handles sending security alerts via Email and other channels.
type AlertManager struct {
	cfg config.AlertConfig
	mu  sync.RWMutex
}

func NewAlertManager(cfg config.AlertConfig) *AlertManager {
	return &AlertManager{
		cfg: cfg,
	}
}

// UpdateConfig dynamically updates SMTP configurations.
func (am *AlertManager) UpdateConfig(cfg config.AlertConfig) {
	am.mu.Lock()
	defer am.mu.Unlock()
	am.cfg = cfg
}

// GetConfig returns current SMTP configuration snapshot.
func (am *AlertManager) GetConfig() config.AlertConfig {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.cfg
}

// MissingFields lists the SMTP settings that prevent alert delivery.
func (am *AlertManager) MissingFields() []string {
	cfg := am.GetConfig()
	var missing []string
	if !cfg.Enabled {
		missing = append(missing, "ALERT_ENABLED")
	}
	if cfg.Host == "" {
		missing = append(missing, "ALERT_SMTP_HOST")
	}
	if cfg.Username == "" {
		missing = append(missing, "ALERT_SMTP_USER")
	}
	if cfg.Password == "" {
		missing = append(missing, "ALERT_SMTP_PASS")
	}
	if cfg.Recipient == "" {
		missing = append(missing, "ALERT_RECIPIENT")
	}
	return missing
}

// SendSecurityEmail sends an HTML-formatted security alert to the administrator
// in the background. Returns ErrAlertNotConfigured when SMTP is not usable.
func (am *AlertManager) SendSecurityEmail(subject, attackIP, alertType, detail, action string) error {
	cfg := am.GetConfig()
	if len(am.MissingFields()) > 0 {
		log.Printf("security: alert email skipped (SMTP not configured or disabled)")
		return ErrAlertNotConfigured
	}
	sender, message := buildSecurityMessage(cfg, subject, attackIP, alertType, detail, action)

	// Sending in background to not block HTTP handlers
	go func() {
		if err := deliverMail(cfg, sender, message); err != nil {
			log.Printf("security: send alert email to %s failed: %v", cfg.Recipient, err)
		} else {
			log.Printf("security: alert email successfully delivered to %s for threat IP %s", cfg.Recipient, attackIP)
		}
	}()
	return nil
}

// SendSecurityEmailSync delivers the alert synchronously and returns the
// SMTP error, so callers such as the test endpoint can report failures.
func (am *AlertManager) SendSecurityEmailSync(subject, attackIP, alertType, detail, action string) error {
	cfg := am.GetConfig()
	if len(am.MissingFields()) > 0 {
		return ErrAlertNotConfigured
	}
	sender, message := buildSecurityMessage(cfg, subject, attackIP, alertType, detail, action)
	if err := deliverMail(cfg, sender, message); err != nil {
		log.Printf("security: send alert email to %s failed: %v", cfg.Recipient, err)
		return err
	}
	log.Printf("security: alert email successfully delivered to %s for threat IP %s", cfg.Recipient, attackIP)
	return nil
}

func buildSecurityMessage(cfg config.AlertConfig, subject, attackIP, alertType, detail, action string) (string, string) {
	body := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background-color: #f6f8fa; margin: 0; padding: 20px; }
  .card { max-width: 600px; margin: 0 auto; background: #ffffff; border-radius: 8px; border: 1px solid #d0d7de; padding: 24px; box-shadow: 0 4px 12px rgba(0,0,0,0.05); }
  .header { border-bottom: 2px solid #cf222e; padding-bottom: 12px; margin-bottom: 20px; }
  .title { color: #cf222e; font-size: 20px; font-weight: 700; margin: 0; }
  .item { margin-bottom: 12px; font-size: 14px; line-height: 1.6; }
  .label { font-weight: 600; color: #24292f; display: inline-block; width: 100px; }
  .val { color: #57606a; }
  .badge { background: #ffebe9; color: #cf222e; padding: 3px 8px; border-radius: 4px; font-weight: 600; border: 1px solid rgba(207,34,46,0.2); }
  .detail { background: #f6f8fa; border: 1px solid #e1e4e8; border-radius: 6px; padding: 12px; font-family: monospace; font-size: 13px; color: #24292f; word-break: break-all; margin-top: 8px; }
  .footer { margin-top: 24px; font-size: 12px; color: #8c959f; border-top: 1px solid #eaecef; padding-top: 12px; text-align: center; }
</style>
</head>
<body>
<div class="card">
  <div class="header">
    <h2 class="title">⚠️ 家庭数据中心安全拦截警报</h2>
  </div>
  <div class="item"><span class="label">警报级别：</span><span class="badge">高危侵入事件</span></div>
  <div class="item"><span class="label">威胁类型：</span><span class="val">%s</span></div>
  <div class="item"><span class="label">攻击来源：</span><span class="val"><strong>%s</strong></span></div>
  <div class="item"><span class="label">触发时间：</span><span class="val">%s</span></div>
  <div class="item"><span class="label">防御动作：</span><span class="val" style="color: #1a7f37; font-weight: 600;">%s</span></div>
  <div class="item">
    <span class="label">行为证据：</span>
    <div class="detail">%s</div>
  </div>
  <div class="footer">
    此邮件由 Home Datacenter 安全防护引擎自动发送，请及时登录管理控制台核查。
  </div>
</div>
</body>
</html>`, alertType, attackIP, time.Now().Format("2006-01-02 15:04:05 MST"), action, detail)

	header := make(map[string]string)
	sender := cfg.Sender
	if sender == "" {
		sender = cfg.Username
	}
	header["From"] = fmt.Sprintf("Home Datacenter Security <%s>", sender)
	header["To"] = cfg.Recipient
	header["Subject"] = fmt.Sprintf("[安全警报] %s: %s", subject, attackIP)
	header["MIME-Version"] = "1.0"
	header["Content-Type"] = "text/html; charset=UTF-8"

	message := ""
	for k, v := range header {
		message += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	message += "\r\n" + body
	return sender, message
}

func deliverMail(cfg config.AlertConfig, sender, message string) error {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	recipients := strings.Split(cfg.Recipient, ",")

	if cfg.Port != 465 {
		// Standard STARTTLS connection (e.g. port 587 / 25)
		return smtp.SendMail(addr, auth, sender, recipients, []byte(message))
	}

	// SSL/TLS direct connection (Standard for QQ Mail port 465)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	if err != nil {
		return fmt.Errorf("TLS dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP client create: %w", err)
	}
	defer client.Quit()

	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("SMTP auth for %s: %w", cfg.Username, err)
	}
	if err := client.Mail(sender); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(strings.TrimSpace(rcpt)); err != nil {
			return fmt.Errorf("SMTP RCPT TO: %w", err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write([]byte(message)); err != nil {
		return fmt.Errorf("SMTP write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close data: %w", err)
	}
	return nil
}
