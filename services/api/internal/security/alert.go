package security

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/config"
)

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

// SendSecurityEmail sends an HTML-formatted security alert to the administrator.
func (am *AlertManager) SendSecurityEmail(subject, attackIP, alertType, detail, action string) error {
	am.mu.RLock()
	cfg := am.cfg
	am.mu.RUnlock()

	if !cfg.Enabled || cfg.Host == "" || cfg.Username == "" || cfg.Password == "" || cfg.Recipient == "" {
		log.Printf("security: alert email skipped (SMTP not configured or disabled)")
		return nil
	}

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

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	// Sending in background to not block HTTP handlers
	go func() {
		var err error
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)

		if cfg.Port == 465 {
			// SSL/TLS direct connection (Standard for QQ Mail port 465)
			tlsconfig := &tls.Config{
				InsecureSkipVerify: false,
				ServerName:         cfg.Host,
			}
			conn, dialErr := tls.Dial("tcp", addr, tlsconfig)
			if dialErr != nil {
				log.Printf("security: TLS dial to %s failed: %v", addr, dialErr)
				return
			}
			client, clientErr := smtp.NewClient(conn, cfg.Host)
			if clientErr != nil {
				log.Printf("security: SMTP client create failed: %v", clientErr)
				return
			}
			defer client.Quit()

			if err = client.Auth(auth); err != nil {
				log.Printf("security: SMTP auth failed for %s: %v", cfg.Username, err)
				return
			}
			if err = client.Mail(sender); err != nil {
				log.Printf("security: SMTP MAIL FROM failed: %v", err)
				return
			}
			if err = client.Rcpt(cfg.Recipient); err != nil {
				log.Printf("security: SMTP RCPT TO failed: %v", err)
				return
			}
			w, dataErr := client.Data()
			if dataErr != nil {
				log.Printf("security: SMTP DATA command failed: %v", dataErr)
				return
			}
			if _, err = w.Write([]byte(message)); err != nil {
				log.Printf("security: SMTP write message failed: %v", err)
				return
			}
			if err = w.Close(); err != nil {
				log.Printf("security: SMTP close data failed: %v", err)
				return
			}
		} else {
			// Standard STARTTLS connection (e.g. port 587 / 25)
			err = smtp.SendMail(addr, auth, sender, strings.Split(cfg.Recipient, ","), []byte(message))
		}

		if err != nil {
			log.Printf("security: send alert email to %s failed: %v", cfg.Recipient, err)
		} else {
			log.Printf("security: alert email successfully delivered to %s for threat IP %s", cfg.Recipient, attackIP)
		}
	}()

	return nil
}
