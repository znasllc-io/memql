package emaildoc

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	_ "image/gif" // the formats an email picture may be
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/znasllc-io/memql/component/compose"
)

// Image is a picture as an ImageResolver hands it over: its bytes and their
// type, image/png, image/jpeg or image/gif.
type Image struct {
	MIMEType string
	Data     []byte
}

// ImageResolver reads the Library pictures a document names as
// artifact:<id>. Render asks for each picture once, with the id after the
// colon, as whoever the resolver reads for. An error means that reader cannot
// have the picture; it becomes an image_unavailable issue, not a failed render.
// The one error that is not that is ErrTooManyPictures.
type ImageResolver interface {
	ResolveImage(ctx context.Context, artifactID string) (Image, error)
}

// ErrTooManyPictures is what an ImageResolver answers (itself or wrapped) for a
// picture it will not read because it has read as many as it is willing to. The
// picture is not at fault, so the render does not say it could not be read: the
// email has more pictures than the reader shows, which is images_limit, said
// once for the email whatever the number of pictures past the limit. The
// picture is not embedded.
var ErrTooManyPictures = errors.New("this reader reads no more pictures")

// picture is what the renderer knows about one image source: the data URI it
// is embedded as and its natural size, or why it cannot be embedded.
type picture struct {
	uri           string // "" when the picture cannot be embedded
	width, height int    // natural size in pixels
	bytes         int    // decoded size
	problem       string // IssueImageUnavailable or IssueImagesLimit when uri is ""
}

// pictures reads, checks and counts the pictures of one render. It reads each
// source once, and counts each distinct picture once, by the data URI it is
// embedded as, against the budget sending allows: MaxImages pictures and
// MaxImageBytes bytes together.
type pictures struct {
	resolver ImageResolver
	looked   map[string]picture // by source
	counted  map[string]bool    // by data URI
	count    int
	total    int
	over     bool // the budget has been exceeded (and said so)
}

func newPictures(resolver ImageResolver) *pictures {
	return &pictures{resolver: resolver, looked: map[string]picture{}, counted: map[string]bool{}}
}

// look returns the picture for a source, artifact:<id> or a data: URI. limit
// is true for the one picture that takes the email past its budget, so the
// issue is said once, however many pictures are over. A picture past the
// budget is still embedded; one that is too large on its own is not. The
// error is a cancelled ctx, and nothing else.
func (p *pictures) look(ctx context.Context, src string) (pic picture, limit bool, err error) {
	pic, seen := p.looked[src]
	if !seen {
		if pic, err = p.read(ctx, src); err != nil {
			return picture{}, false, err
		}
		p.looked[src] = pic
	}
	switch {
	case pic.problem == IssueImagesLimit:
		limit = !p.over
		p.over = true
	case pic.uri != "" && !p.counted[pic.uri]:
		p.counted[pic.uri] = true
		p.count++
		p.total += pic.bytes
		if !p.over && (p.count > MaxImages || p.total > MaxImageBytes) {
			p.over, limit = true, true
		}
	}
	return pic, limit, nil
}

// read reads and checks a picture the first time its source comes up.
func (p *pictures) read(ctx context.Context, src string) (picture, error) {
	unavailable := picture{problem: IssueImageUnavailable}
	if id, ok := strings.CutPrefix(src, "artifact:"); ok {
		if p.resolver == nil || !artifactPattern.MatchString(src) {
			return unavailable, nil
		}
		if err := ctx.Err(); err != nil {
			return picture{}, err
		}
		img, err := p.resolver.ResolveImage(ctx, id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return picture{}, ctxErr
			}
			if errors.Is(err, ErrTooManyPictures) {
				return picture{problem: IssueImagesLimit}, nil
			}
			return unavailable, nil
		}
		return raster(img.MIMEType, img.Data, ""), nil
	}
	// An embedded picture is read the way sending reads it
	// (compose.ExtractEmailImages), so a picture this accepts is one sending
	// accepts. It is a data: URI of an image, or nothing: without the scheme
	// "image/png;base64,..." would be written as a web address.
	src = strings.TrimSpace(src)
	if !strings.HasPrefix(src, "data:image/") {
		return unavailable, nil
	}
	prefix, encoded, ok := strings.Cut(src, ",")
	if !ok || !strings.HasSuffix(prefix, ";base64") || strings.ContainsAny(encoded, "\r\n") {
		return unavailable, nil
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxImageBytes) {
		return picture{problem: IssueImagesLimit}, nil
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return unavailable, nil
	}
	return raster(strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64"), data, src), nil
}

// raster checks picture bytes as sending does and describes them. uri is the
// source the bytes came from when it is already a data URI.
func raster(mime string, data []byte, uri string) picture {
	if len(data) > MaxImageBytes {
		return picture{problem: IssueImagesLimit}
	}
	if compose.EmailImageData(data, mime) != nil {
		return picture{problem: IssueImageUnavailable}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return picture{problem: IssueImageUnavailable}
	}
	if uri == "" {
		uri = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return picture{uri: uri, width: config.Width, height: config.Height, bytes: len(data)}
}

// scaledHeight is the height of a picture shown width pixels wide at its
// natural ratio, rounded to the nearest pixel.
func (pic picture) scaledHeight(width int) int {
	return (2*width*pic.height + pic.width) / (2 * pic.width)
}

// The sentences of the picture issues.
func pictureMissing(label string) string {
	return `Choose a picture for "` + clip(label, 60) + `".`
}

func pictureUnavailable(label string) string {
	return `The picture for "` + clip(label, 60) + `" could not be read. Choose it again.`
}

const picturesOverBudget = "Use up to eight pictures totaling 1 MiB."
