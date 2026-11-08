// Package server holds the script that runs on the VPS.
package server

import _ "embed"

// Script is installed on the VPS as /usr/local/sbin/dagon.
//
//go:embed dagon.sh
var Script []byte
