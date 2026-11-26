package camera

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDaguFetchDayRecordingsParser(t *testing.T) {
	mockHTML := `<html><head><title>cloud.daguiot.com - /4/15/20261121/6912200023929742/video/</title></head><body>
<pre><A HREF="/4/15/20261121/6912200023929742/">[转到父目录]</A><br><br>
<A HREF="/4/15/20261121/6912200023929742/video/1795273941_003_5">1795273941_003_5</A><br>
<A HREF="/4/15/20261121/6912200023929742/video/1795274965_006_5">1795274965_006_5</A><br>
<A HREF="/4/15/20261121/6912200023929742/video/1795275541_032_1">1795275541_032_1</A><br>
</pre></body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "video") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(mockHTML))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	matches := daguRecRegex.FindAllStringSubmatch(mockHTML, -1)
	if len(matches) != 3 {
		t.Fatalf("expected 3 matches, got %d", len(matches))
	}

	if matches[0][1] != "1795273941" || matches[0][2] != "003" || matches[0][3] != "5" {
		t.Errorf("unexpected first match: %v", matches[0])
	}
	if matches[2][1] != "1795275541" || matches[2][2] != "032" || matches[2][3] != "1" {
		t.Errorf("unexpected third match: %v", matches[2])
	}
}

func TestDaguListRecordingsLive(t *testing.T) {
	mgr := NewDaguManager("15291472514", "wm10050817")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sn := "6912200023929742"
	now := time.Now().Unix()
	after := now - 2*24*3600 // last 2 days

	recs, err := mgr.ListRecordings(ctx, sn, after, now)
	if err != nil {
		t.Logf("Live query notice (network dependent): %v", err)
		return
	}

	t.Logf("Fetched %d cloud recordings from Dagu cloud", len(recs))
	if len(recs) > 0 {
		t.Logf("First recording: %s (start: %d, duration: %ds, url: %s)",
			recs[0].Filename, recs[0].StartUnix, recs[0].DurationSeconds, recs[0].URL)
	}
}

func TestGetRecordingCacheDir(t *testing.T) {
	dir := GetRecordingCacheDir(21)
	if dir == "" {
		t.Fatal("expected non-empty cache dir")
	}
	t.Logf("Cache dir for cam 21: %s", dir)
	_ = os.RemoveAll(dir)
}
