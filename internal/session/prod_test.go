package session

import (
	"reflect"
	"testing"
)

func TestParseSessions(t *testing.T) {
	cases := []struct {
		name          string
		out           string
		active, stale []string
		ok            bool
	}{
		{"空输出=无会话", "", nil, nil, true},
		{"仅换行", "\n\n", nil, nil, true},
		{"活跃会话", "ACTIVE sabc\n", []string{"sabc"}, nil, true},
		{"遗留会话", "STALE sold\n", nil, []string{"sold"}, true},
		{"混合与空行", "ACTIVE a\n\nSTALE b\n", []string{"a"}, []string{"b"}, true},
		{"未知前缀", "WEIRD x\n", nil, nil, false},
		{"空 id", "ACTIVE \n", nil, nil, false},
		{"无空格分隔", "ACTIVEx\n", nil, nil, false},
		{"混入诊断文本", "ACTIVE a\nsome diagnostic noise\n", []string{"a"}, nil, false},
		{"id 含空格", "ACTIVE a b\n", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			active, stale, ok := ParseSessions(tc.out)
			if ok != tc.ok {
				t.Fatalf("ok=%v 期望 %v (out=%q)", ok, tc.ok, tc.out)
			}
			if ok && (!reflect.DeepEqual(active, tc.active) || !reflect.DeepEqual(stale, tc.stale)) {
				t.Fatalf("active=%v stale=%v 期望 %v/%v", active, stale, tc.active, tc.stale)
			}
		})
	}
}
