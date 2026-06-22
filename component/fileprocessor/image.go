package fileprocessor

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/core/common"
)

const (
	visionPrompt  = "Describe the content of this image in detail. Include all visible text, charts, diagrams, and key visual elements. Be concise but complete."
	maxImageBytes = 20 * 1024 * 1024 // 20 MB
)

// describeImage uses a VisionAIProvider to describe the image content as text.
func describeImage(ctx context.Context, visionProvider common.VisionAIProvider, mimeType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if visionProvider == nil {
		return "", fmt.Errorf("no vision provider configured for image description")
	}
	if len(data) > maxImageBytes {
		data = data[:maxImageBytes]
	}

	return visionProvider.CallVision(ctx, visionPrompt, []common.VisionContent{
		{MimeType: mimeType, Data: data},
	})
}
