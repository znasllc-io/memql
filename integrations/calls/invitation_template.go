package calls

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/url"

	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/integrations/email"
)

// Content is fixed and product-neutral. Branding is the checked deployable's title
// and bundled favicon.png on its canonical public host, never browser-supplied HTML,
// from-address, host capability or arbitrary image URL. No tracking is inserted.
var invitationHTML = template.Must(template.New("call-invitation").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Subject}}</title></head><body style="margin:0;background:#f8fafc;color:#17232b;font-family:Arial,sans-serif;line-height:1.6"><table role="presentation" style="width:100%;border-collapse:collapse"><tr><td style="padding:32px 20px"><table role="presentation" style="width:100%;max-width:520px;margin:auto;border-collapse:collapse"><tr><td style="padding-bottom:28px;font-size:20px;font-weight:600"><img src="{{.Logo}}" width="32" height="32" alt="" style="vertical-align:middle;margin-right:8px">{{.Brand}}</td></tr><tr><td><h1 style="margin:0 0 12px;font-size:28px;font-weight:400;line-height:1.2">You're invited to a call.</h1><p style="margin:0 0 24px">Join the conversation. No account is needed.</p><p style="margin:0 0 24px"><a href="{{.Link}}" style="display:inline-block;background:#0b716b;color:#fff;padding:12px 24px;border-radius:8px;text-decoration:none;font-weight:600">Join call</a></p><p style="font-size:13px;color:#53616c">The host may ask you to wait before joining.</p><p style="margin-top:28px;padding-top:20px;border-top:1px solid #dce3e7;font-size:12px;color:#53616c">Or copy this link:<br><a href="{{.Link}}" style="color:#0b716b;overflow-wrap:anywhere">{{.Link}}</a></p><p style="font-size:12px;color:#53616c">If you weren't expecting this invitation, you can ignore it.</p></td></tr></table></td></tr></table></body></html>`))

func renderInvitation(brand, host, recipient, token string) (email.Message, error) {
	u, err := url.Parse("https://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !callrules.TokenWellFormed(token) {
		return email.Message{}, fmt.Errorf("calls: invalid invitation host or token")
	}
	brand = invitationBrand(brand)
	link := callrules.GuestLink(host, token)
	subject := "You're invited to a " + brand + " call"
	var html bytes.Buffer
	err = invitationHTML.Execute(&html, map[string]string{"Subject": subject, "Brand": brand, "Link": link, "Logo": "https://" + host + "/favicon.png"})
	if err != nil {
		return email.Message{}, fmt.Errorf("calls: invitation template could not render")
	}
	return email.Message{To: recipient, Subject: subject, HTMLBody: html.String(), TextBody: fmt.Sprintf("%s\n\nYou're invited to a call.\n\nJoin call:\n%s\n\nNo account is needed. The host may ask you to wait before joining.\n\nIf you weren't expecting this invitation, you can ignore it.", brand, link)}, nil
}

func (s *Store) invitationMessage(ctx context.Context, site callrules.SiteRef, recipient, token string) (email.Message, error) {
	host, err := s.publicHost(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return email.Message{}, err
	}
	call, err := langparser.RenderCall("siteById", map[string]any{"siteId": site.SiteID})
	rows, err := s.readAs(ctx, site.OwnerUserID, "the invitation's deployable", call, err)
	if err != nil || len(rows) == 0 {
		return email.Message{}, callrules.ErrInvitationUnavailable
	}
	brand := rowString(rows[0], "title")
	if brand == "" {
		brand = site.Deployable
	}
	return renderInvitation(brand, host, recipient, token)
}
