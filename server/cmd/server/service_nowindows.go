//go:build !windows

package main

// 非 Windows 平台没有 SCM 形态，恒为 false。
func runAsWindowsService() bool { return false }
