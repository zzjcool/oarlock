package acp

import "github.com/zzjcool/oarlock"

func init() {
	for _, rt := range Runtimes() {
		oarlock.Register(rt)
	}
}
