package operations

import "strings"

// ApplicationSourceExclusions are supplied by the protected checkout, never by
// the requested application revision. Application build recipes remain versioned.
func ApplicationSourceExclusions() []string {
	return []string{
		"devops", "cicd", ".gitlab", ".gitlab-ci.yml",
		"scripts/deploy", "scripts/operations",
		"infrastructure/environments", "infrastructure/nginx", "infrastructure/scripts",
		"infrastructure/legacy", "infrastructure/README.md",
	}
}

func applicationSourcePath(name string) bool {
	for _, excluded := range ApplicationSourceExclusions() {
		if name == excluded || strings.HasPrefix(name, excluded+"/") {
			return false
		}
	}
	if strings.HasPrefix(name, "infrastructure/") {
		return name == "infrastructure/application" || strings.HasPrefix(name, "infrastructure/application/")
	}
	return true
}
