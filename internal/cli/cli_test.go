package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/liquidcatmofu/mcwarden/internal/app"
)

func TestRun(t *testing.T) {
	for _, tt := range []struct { args []string; code int; json bool }{
		{[]string{"help"}, 0, false},
		{[]string{"version"}, 0, false},
		{[]string{"unknown"}, 2, false},
		{[]string{"status", "--json"}, 5, true},
		{[]string{"stop", "--json"}, 5, true},
		{[]string{"restart", "--kill", "--json"}, 2, true},
		{[]string{"restart", "--force", "--json"}, 2, true},
		{[]string{"restart", "--kill", "--force", "--json"}, 5, true},
		{[]string{"stop", "--stop-timeout=0s", "--json"}, 2, true},
		{[]string{"stop", "--term"}, 2, false},
		{[]string{"restart", "-y"}, 2, false},
		{[]string{"status", "--json", "extra"}, 2, true},
	} { t.Run(join(tt.args), func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Run(context.Background(), tt.args, &out, &stderr, app.Service{})
		if code != tt.code { t.Fatalf("code=%d want=%d stdout=%s stderr=%s", code, tt.code, &out, &stderr) }
		if tt.json {
			var result response
			if err := json.Unmarshal(out.Bytes(), &result); err != nil { t.Fatal(err) }
			if result.ExitCode != code || (code != 0 && result.Error == "") || stderr.Len() != 0 { t.Fatalf("response=%+v stderr=%s", result, &stderr) }
		}
	}) }
}

func join(args []string) string { b, _ := json.Marshal(args); return string(b) }
