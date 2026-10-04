package toolcall

import (
	"strings"
	"testing"
)

func TestNormalizeJSONShellCmdToCommand(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"cmd only", `{"cmd":"pwd"}`, `"command"`},
		{"command only", `{"command":"pwd"}`, `"command"`},
		{"exec_command tool", `{"cmd":"curl wttr.in/Changsha"}`, `"command"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := "shell"
			if strings.Contains(c.name, "exec") {
				tool = "exec_command"
			}
			got := NormalizeJSON(c.in, tool)
			t.Logf("got=%s", got)
			if !strings.Contains(got, c.want) {
				t.Fatalf("expected %s in %s", c.want, got)
			}
			if strings.Contains(got, `"cmd"`) {
				t.Fatalf("cmd alias leaked: %s", got)
			}
		})
	}
}
