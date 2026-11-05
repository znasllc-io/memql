package compose

import (
	"errors"

	pure "github.com/znasllc-io/memql/component/compose"
)

// ErrEmailsElsewhere is the refusal every way of asking the Materializer for an
// email gets. An email is composed in the Email app: a person picks a layout,
// the template is created with the organization's brand kit applied, and the
// content is written in the background (component/campaigns).
var ErrEmailsElsewhere = errors.New("Emails are made in the Email app.")

// EmailAssetHandle is the name a picture selected for inclusion is offered to
// a composer by, and the only name it can put a picture in an email by: the
// digest of its captured bytes, so a handle names those bytes and no others.
// The Email app's composition (component/campaigns) offers the same handles.
func EmailAssetHandle(image []byte) string {
	return "memql-asset:" + (pure.Result{Bytes: image}).SHA256()
}
