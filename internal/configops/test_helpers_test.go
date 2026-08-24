package configops

func int64ptr(value int64) *int64 { return &value }

func strptr(value string) *string { return &value }

func boolptr(value bool) *bool { return &value }
