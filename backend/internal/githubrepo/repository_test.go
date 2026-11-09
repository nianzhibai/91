package githubrepo

import "testing"

func TestRepositoryURL(t *testing.T) {
	for _, address := range []string{"https://github.com/owner/web", "https://github.com/owner/web/", "https://github.com/owner/web.git"} {
		if name, err := Name(address); err != nil || name != "owner/web" {
			t.Fatalf("%s: %q %v", address, name, err)
		}
	}
	for _, address := range []string{"owner/web", "http://github.com/owner/web", "https://evil.test/owner/web", "https://github.com/owner/web/tree/main", "https://user@github.com/owner/web", "https://github.com:443/owner/web", "https://github.com/owner/web?x=1", "https://github.com/owner/web#readme", "https://github.com/owner%2fweb", "https://github.com/owner/.."} {
		if _, err := Name(address); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
}
