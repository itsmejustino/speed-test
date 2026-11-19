package main

import "strings"

// Marks a token encrypted for this Windows account (see secret_windows.go).
const secretPrefix = "dpapi:"

func isProtected(s string) bool { return strings.HasPrefix(s, secretPrefix) }
