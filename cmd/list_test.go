package cmd

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/katakalyst/devsys/internal/podmanfake"
)

// captureStdout redirects os.Stdout to a pipe for the duration of the test
// and returns a function that closes the write end and reads all captured output.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("captureStdout: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })
	return func() string {
		w.Close()
		os.Stdout = orig
		b, _ := io.ReadAll(r)
		return string(b)
	}
}

// ---------------------------------------------------------------------------
// containerField
// ---------------------------------------------------------------------------

func TestContainerField_StringValue(t *testing.T) {
	m := map[string]interface{}{"Names": "devsys-test"}
	if got := containerField(m, "Names"); got != "devsys-test" {
		t.Errorf("want %q, got %q", "devsys-test", got)
	}
}

func TestContainerField_SliceValue(t *testing.T) {
	m := map[string]interface{}{"Names": []interface{}{"devsys-test"}}
	if got := containerField(m, "Names"); got != "devsys-test" {
		t.Errorf("want %q, got %q", "devsys-test", got)
	}
}

func TestContainerField_EmptySlice(t *testing.T) {
	// An empty slice has no first element — containerField falls through to
	// fmt.Sprintf("%v", v) which formats it as "[]".
	m := map[string]interface{}{"Names": []interface{}{}}
	if got := containerField(m, "Names"); got != "[]" {
		t.Errorf("want %q for empty slice, got %q", "[]", got)
	}
}

func TestContainerField_MissingKey(t *testing.T) {
	m := map[string]interface{}{"Other": "value"}
	if got := containerField(m, "Names"); got != "" {
		t.Errorf("want empty string for missing key, got %q", got)
	}
}

func TestContainerField_NonStringValue(t *testing.T) {
	m := map[string]interface{}{"Count": 42}
	if got := containerField(m, "Count"); got != "42" {
		t.Errorf("want %q, got %q", "42", got)
	}
}

// ---------------------------------------------------------------------------
// runList
// ---------------------------------------------------------------------------

func TestRunList_Empty(t *testing.T) {
	podmanfake.Install(t, podmanfake.Options{})
	flush := captureStdout(t)

	if err := runList(nil, nil); err != nil {
		t.Fatalf("runList: %v", err)
	}

	out := flush()
	if !strings.Contains(out, "No devsys containers found") {
		t.Errorf("expected 'No devsys containers found' in output, got: %q", out)
	}
	if !strings.Contains(out, "No devsys volumes found") {
		t.Errorf("expected 'No devsys volumes found' in output, got: %q", out)
	}
}
