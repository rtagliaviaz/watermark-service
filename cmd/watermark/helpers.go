package main

// trimTrailingSlash removes any trailing '/' characters from s. It is
// shared by ab-prepare and ab-assemble to normalize the --license-url
// flag before building a key URI.
func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
