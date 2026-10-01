package updater

import "testing"

func TestForkUIURL(t *testing.T) {
	for _, name := range []string{"dist.zip", "dist-cdn-fonts.zip"} {
		old := "https://github.com/Zephyruso/zashboard/releases/latest/download/" + name
		want := "https://github.com/x-happy-x/zashboard/releases/latest/download/" + name
		if got := NewUiUpdater("", old, "").externalUIURL; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	for _, raw := range []string{"https://example.org/custom.zip", "https://github.com/Zephyruso/zashboard/releases/download/v3.0.0/dist.zip", "https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip?custom=true"} {
		if got := forkUIURL(raw); got != raw {
			t.Fatalf("custom URL changed: %s", got)
		}
	}
}
