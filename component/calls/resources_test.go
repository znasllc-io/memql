package calls

import "testing"

func TestResourceOutputBounds(t *testing.T) {
	base := ResourceSpec{Label: "Interview whiteboard", Kind: "terminal", Video: true, Audio: true, MaxWidth: 1280, MaxHeight: 720, MaxFPS: 15, MaxBitrateKbps: 1000}
	if !ValidResourceSpec(base) {
		t.Fatal("bounded output rejected")
	}
	for name, mutate := range map[string]func(*ResourceSpec){"no tracks": func(r *ResourceSpec) { r.Video = false }, "unbounded fps": func(r *ResourceSpec) { r.MaxFPS = 0 }, "4k": func(r *ResourceSpec) { r.MaxWidth = 3840 }, "high bitrate": func(r *ResourceSpec) { r.MaxBitrateKbps = 1001 }, "input URL": func(r *ResourceSpec) { r.Kind = "rtsp://camera" }, "control label": func(r *ResourceSpec) { r.Label = "secret\nnew line" }} {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if ValidResourceSpec(r) {
				t.Fatal("unsafe output accepted")
			}
		})
	}
}
func TestResourceScopesDefaultDeny(t *testing.T) {
	if ResourceScope("send_device_command") != "" {
		t.Fatal("device commands acquired publication authority")
	}
	if ResourceScope("start") != "resources_publish" || ResourceScope("stop") != "resources_control" || ResourceScope("list") != "resources_read" {
		t.Fatal("resource authority drift")
	}
}
