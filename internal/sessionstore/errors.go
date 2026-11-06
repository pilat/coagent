package sessionstore

import "errors"

var ErrOutputNotWritable = errors.New("cannot commit ordinary output")
