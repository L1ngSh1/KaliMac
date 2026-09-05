package runtime

import (
	"errors"
	"testing"
)

func TestClassifyCommandError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"无 docker CLI", RunErr(`exec: "docker": executable file not found in $PATH`, -1), CodeRuntimeMissing},
		{"引擎离线", RunErr("Cannot connect to the Docker daemon at unix:///x.sock. Is the docker daemon running?", 1), CodeRuntimeOffline},
		{"socket 缺失", RunErr("dial unix /x.sock: connect: no such file or directory", 1), CodeRuntimeOffline},
		{"容器内命令缺失", RunErr(`OCI runtime exec failed: exec failed: unable to start container process: exec: "nope": executable file not found in $PATH`, 127), CodeRuntimeMissing},
		{"容器不存在", RunErr("Error response from daemon: No such container: km-p1", 1), CodeNotFound},
		{"镜像不存在", RunErr("Error response from daemon: No such image: x:latest", 1), CodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCommandError(tc.err)
			var kmerr *Error
			if !errors.As(got, &kmerr) {
				t.Fatalf("未产生 KM 错误: %v", got)
			}
			if kmerr.Code != tc.want {
				t.Fatalf("code = %s, 期望 %s (err=%v)", kmerr.Code, tc.want, got)
			}
		})
	}
}

func TestClassifyNil(t *testing.T) {
	if err := ClassifyCommandError(nil); err != nil {
		t.Fatalf("nil 应返回 nil, got %v", err)
	}
}
