//go:build linux

package pigeon

import (
	"fmt"
	"os"
	"strings"
)

// ProcStart reads field 22 (starttime) of /proc/<pid>/stat, which lets us tell
// a reused PID from the original process. comm (field 2) may itself contain
// spaces and parentheses, so scan past the final ')'.
func ProcStart(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return ""
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
