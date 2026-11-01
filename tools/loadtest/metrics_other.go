//go:build !windows

package main

func platformRSSMB() float64 { return -1 }

func platformHandles() int { return -1 }
