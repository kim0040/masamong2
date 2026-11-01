//go:build !noweb

package main

// webEnabled is compiled in when templates are embedded in the daemon; CLI still does not listen.
const webEnabled = true
