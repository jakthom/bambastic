//go:build !linux

package spoolbuddy

func PiHardware() Hardware { return Hardware{} }
