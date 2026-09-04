//go:build !darwin && !linux && !windows

package main

import "hearth/server/internal/config"

// 其余平台暂不支持服务化：runServiceCmd 会直接提示。
const serviceSupported = false

func svcInstall(cfg config.Config, system bool) error   { return nil }
func svcUninstall(cfg config.Config, system bool) error { return nil }
func svcStart(system bool) error                        { return nil }
func svcStop(system bool) error                         { return nil }
func svcStatus(system bool) error                       { return nil }
