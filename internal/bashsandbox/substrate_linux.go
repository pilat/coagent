//go:build linux

package bashsandbox

func runtimeDirectoryCandidates() []string {
	return []string{
		"/bin", "/sbin", "/lib", "/lib64", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64",
		"/usr/libexec", "/usr/share", "/nix/store", "/run/current-system", "/run/opengl-driver",
		"/etc/ld.so.conf.d", "/etc/ssl/certs", "/etc/pki/tls/certs",
	}
}

func runtimeFileCandidates() []string {
	return []string{
		"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/resolv.conf", "/etc/hosts",
		"/etc/nsswitch.conf", "/etc/gai.conf", "/etc/services", "/etc/protocols",
		"/etc/passwd", "/etc/group", "/etc/localtime",
	}
}
