// Package mailer sends installation and notification email through a configured SMTP server.
package mailer

import (
	"bambastic/internal/lanhttp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host, From, Security, Username, Password string
	Port                                     int
	Auth                                     bool
}
type Error struct {
	Status  int
	Message string
}

func (e Error) Error() string         { return e.Message }
func fail(code int, msg string) error { return Error{code, msg} }
func Send(ctx context.Context, c Config, toAddress, title, message string) error {
	host := strings.TrimSpace(c.Host)
	port := c.Port
	if port == 0 {
		port = 587
	}
	if port < 1 || port > 65535 || strings.ContainsAny(host, "\r\n/") {
		return fail(422, "Invalid SMTP endpoint")
	}
	sender, e := mail.ParseAddress(c.From)
	if e != nil {
		return fail(422, "Invalid sender address")
	}
	to, e := mail.ParseAddressList(toAddress)
	if e != nil || len(to) == 0 {
		return fail(422, "Invalid recipient address")
	}
	if strings.ContainsAny(title, "\r\n") {
		title = strings.Join(strings.Fields(title), " ")
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, e := lanhttp.DialContext(ctx, "tcp", addr)
	if e != nil {
		return fail(503, "Cannot reach SMTP server")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	security := c.Security
	if security == "" {
		security = "starttls"
	}
	if security == "ssl" || security == "tls" {
		conn = tls.Client(conn, cfg)
	}
	client, e := smtp.NewClient(conn, host)
	if e != nil {
		return fail(502, "SMTP connection failed")
	}
	defer client.Close()
	if security == "starttls" {
		if e = client.StartTLS(cfg); e != nil {
			return fail(502, "SMTP STARTTLS failed")
		}
	} else if security != "ssl" && security != "tls" && security != "none" {
		return fail(422, "Unknown SMTP security setting")
	}
	if c.Auth {
		u, p := c.Username, c.Password
		if u == "" || p == "" {
			return fail(422, "SMTP credentials required")
		}
		if e = client.Auth(smtp.PlainAuth("", u, p, host)); e != nil {
			return fail(502, "SMTP authentication failed")
		}
	}
	if e = client.Mail(sender.Address); e != nil {
		return fail(502, "SMTP sender rejected")
	}
	var labels []string
	for _, a := range to {
		if e = client.Rcpt(a.Address); e != nil {
			return fail(502, "SMTP recipient rejected")
		}
		labels = append(labels, a.String())
	}
	w, e := client.Data()
	if e != nil {
		return fail(502, "SMTP message refused")
	}
	raw := base64.StdEncoding.EncodeToString([]byte(message))
	var body strings.Builder
	for len(raw) > 76 {
		body.WriteString(raw[:76] + "\r\n")
		raw = raw[76:]
	}
	body.WriteString(raw)
	_, e = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", sender.String(), strings.Join(labels, ", "), mime.QEncoding.Encode("utf-8", title), time.Now().Format(time.RFC1123Z), body.String())
	if e != nil {
		return fail(502, "SMTP message write failed")
	}
	if e = w.Close(); e != nil {
		return fail(502, "SMTP server did not accept message")
	}
	return nil
}
