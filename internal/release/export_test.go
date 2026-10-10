package release

// ReplaceHook exposes replace's afterClose hook to release_test.
func ReplaceHook(exe string, bin []byte, afterClose func(tmp string)) error {
	return replace(exe, bin, afterClose)
}

// Extract exposes extract; MaxUnpacked its bound.
var Extract = extract

const MaxUnpacked = maxUnpacked
