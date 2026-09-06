//go:build darwin

package bashsandbox

func runtimeDirectoryCandidates() []string {
	return []string{
		"/System", "/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/libexec", "/usr/share",
		"/Library/Apple", "/Library/Filesystems/NetFSPlugins", "/Library/Preferences/Logging",
		"/Library/Preferences", "/var/db", "/private/var/db", "/private/var/db/timezone",
		"/private/preboot/Cryptexes/OS", "/Library/Developer/CommandLineTools",
		"/Applications/Xcode.app/Contents", "/private/var/db/xcode_select_link",
	}
}

func runtimeFileCandidates() []string {
	return []string{
		"/etc/resolv.conf", "/etc/hosts", "/etc/services", "/etc/protocols", "/etc/localtime",
		"/etc/passwd", "/etc/master.passwd", "/etc/ssl/openssl.cnf", "/etc/ssl/cert.pem",
		"/private/var/db/DarwinDirectory/local/recordStore.data",
		"/private/var/db/eligibilityd/eligibility.plist", "/private/var/select/sh", "/dev/autofs_nowait",
	}
}
