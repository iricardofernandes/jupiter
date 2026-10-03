package secureurl

import "testing"

func TestCheck(t *testing.T) {
	for _, ok := range []string{"https://ds.example.com/areq", "http://127.0.0.1:8585/ds", "http://localhost/x", "http://[::1]:80/"} {
		if err := Check(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://ds.example.com/areq", "ftp://127.0.0.1/", "/relative", "https://user:pw@ds.example.com/", "", "http://127.0.0.1.example.com/"} {
		if err := Check(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
