package email

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// Every SMTP fixture is a single loopback connection and never relays mail.
type smtpContextPeer struct {
	t      *testing.T
	conn   net.Conn
	reader *textproto.Reader
	writer *bufio.Writer
}

func (p *smtpContextPeer) reply(line string) bool {
	if _, err := p.writer.WriteString(line + "\r\n"); err != nil {
		p.t.Errorf("SMTP fixture write: %v", err)
		return false
	}
	if err := p.writer.Flush(); err != nil {
		p.t.Errorf("SMTP fixture flush: %v", err)
		return false
	}
	return true
}

func (p *smtpContextPeer) command(prefix string) bool {
	line, err := p.reader.ReadLine()
	if err != nil || !strings.HasPrefix(line, prefix) {
		p.t.Errorf("SMTP command = %q, error = %v; want prefix %q", line, err, prefix)
		return false
	}
	return true
}

func smtpContextFixture(t *testing.T, serve func(*smtpContextPeer)) SMTPConfig {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		serve(&smtpContextPeer{t: t, conn: connection, reader: textproto.NewReader(bufio.NewReader(connection)), writer: bufio.NewWriter(connection)})
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("SMTP fixture did not close")
		}
	})
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return SMTPConfig{Host: host, Port: port, FromAddr: "sender@example.test", FromName: "Test sender"}
}

func smtpContextEnvelope(p *smtpContextPeer, auth bool) bool {
	if !p.reply("220 localhost SMTP fixture") || !p.command("EHLO localhost") {
		return false
	}
	if auth {
		if !p.reply("250-localhost\r\n250 AUTH PLAIN") || !p.command("AUTH PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00fixture-user\x00fixture-password"))) || !p.reply("235 authenticated") {
			return false
		}
	} else if !p.reply("250 localhost") {
		return false
	}
	return p.command("MAIL FROM:<sender@example.test>") && p.reply("250 sender accepted") &&
		p.command("RCPT TO:<recipient@example.test>") && p.reply("250 recipient accepted") &&
		p.command("DATA") && p.reply("354 send message")
}

var smtpContextMessage = Message{To: "recipient@example.test", Subject: "Fixture", TextBody: "Local fixture body"}

func TestSMTPCancellationInterruptsGreetingBeforeSubmission(t *testing.T) {
	ready := make(chan struct{})
	config := smtpContextFixture(t, func(p *smtpContextPeer) {
		close(ready)
		_, _ = io.Copy(io.Discard, p.conn) // no greeting, until the client cancels
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- NewSMTPSender(config, nil).Send(ctx, smtpContextMessage, SendAs{}) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("SMTP connection did not open")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !IsNotAccepted(err) {
			t.Fatalf("cancelled greeting must be not accepted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP greeting ignored cancellation")
	}
}

func TestSMTPDeadlineBoundsGreeting(t *testing.T) {
	config := smtpContextFixture(t, func(p *smtpContextPeer) { _, _ = io.Copy(io.Discard, p.conn) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := NewSMTPSender(config, nil).Send(ctx, smtpContextMessage, SendAs{})
	if !errors.Is(err, context.DeadlineExceeded) || !IsNotAccepted(err) {
		t.Fatalf("expired greeting must be not accepted: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("SMTP greeting exceeded its deadline")
	}
}

func TestSMTPCancelledDataAcknowledgmentRemainsUncertain(t *testing.T) {
	submitted := make(chan struct{})
	config := smtpContextFixture(t, func(p *smtpContextPeer) {
		if !smtpContextEnvelope(p, false) {
			return
		}
		if _, err := p.reader.ReadDotBytes(); err != nil {
			t.Errorf("read submitted body: %v", err)
			return
		}
		close(submitted)
		_, _ = io.Copy(io.Discard, p.conn) // body arrived; never acknowledge it
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- NewSMTPSender(config, nil).Send(ctx, smtpContextMessage, SendAs{}) }()
	select {
	case <-submitted:
	case <-time.After(time.Second):
		t.Fatal("SMTP message did not reach DATA")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || IsNotAccepted(err) {
			t.Fatalf("a lost DATA acknowledgment must remain uncertain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP DATA acknowledgment ignored cancellation")
	}
}

func TestSMTPAcceptanceSurvivesQuitFailure(t *testing.T) {
	for _, dropAfterAcceptance := range []bool{false, true} {
		name := "normal completion"
		if dropAfterAcceptance {
			name = "connection closes after 250"
		}
		t.Run(name, func(t *testing.T) {
			config := smtpContextFixture(t, func(p *smtpContextPeer) {
				if !smtpContextEnvelope(p, true) {
					return
				}
				body, err := p.reader.ReadDotBytes()
				if err != nil {
					t.Errorf("read SMTP body: %v", err)
					return
				}
				for _, want := range []string{"From: Test sender <sender@example.test>", "To: recipient@example.test", "Subject: Fixture", "Local fixture body"} {
					if !strings.Contains(string(body), want) {
						t.Errorf("rendered MIME is missing %q", want)
					}
				}
				if !p.reply("250 message accepted") || dropAfterAcceptance {
					return
				}
				if p.command("QUIT") {
					p.reply("221 goodbye")
				}
			})
			config.Username, config.Password = "fixture-user", "fixture-password"
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := NewSMTPSender(config, nil).Send(ctx, smtpContextMessage, SendAs{}); err != nil {
				t.Fatalf("accepted SMTP message failed: %v", err)
			}
		})
	}
}

func TestSMTPStartTLSKeepsCertificateVerification(t *testing.T) {
	certificateSource := httptest.NewTLSServer(nil)
	certificate := certificateSource.TLS.Certificates[0]
	certificateSource.Close()
	config := smtpContextFixture(t, func(p *smtpContextPeer) {
		if !p.reply("220 localhost SMTP fixture") || !p.command("EHLO localhost") || !p.reply("250-localhost\r\n250 STARTTLS") || !p.command("STARTTLS") || !p.reply("220 begin TLS") {
			return
		}
		secured := tls.Server(p.conn, &tls.Config{Certificates: []tls.Certificate{certificate}})
		if err := secured.Handshake(); err == nil {
			t.Error("SMTP client trusted an unrecognized test certificate")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := NewSMTPSender(config, nil).Send(ctx, smtpContextMessage, SendAs{})
	if err == nil || !IsNotAccepted(err) || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted STARTTLS certificate was not refused before sending: %v", err)
	}
}
