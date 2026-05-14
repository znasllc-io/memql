package gcs

import (
	"context"

	"github.com/visionarys-io/memql/component/memql"
)

// init self-registers the GCS storage integration as a plug-in. Only
// materializes when a bucket is configured via environment. Absent
// configuration is silent; uploader-construction failures log a warning
// and opt the plug-in out (no hard failure -- storage is optional).
func init() {
	memql.RegisterPlugin("storage", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		bucket := BucketFromEnv()
		if bucket == "" {
			return nil, nil
		}
		uploader, err := New(context.Background())
		if err != nil {
			pctx.Logger.Warn("GCS storage plug-in opting out", "error", err)
			return nil, nil
		}
		return NewStorageIntegration(uploader, bucket), nil
	})
}
