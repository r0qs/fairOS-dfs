package common

import "os"

const (
	MaxChunkLength         = 4096
	FileNameLength         = 50
	MaxDirectoryNameLength = 50
	PathSeperator          = string(os.PathSeparator)
	MaxPodNameLength       = 25
	SpanLength             = 8
)
