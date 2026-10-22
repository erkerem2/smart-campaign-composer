package segment

import "testing"

func TestNormalizeCity(t *testing.T) {
	cases := map[string]string{
		"İstanbul":            "İstanbul",
		"Istanbul":            "İstanbul",
		"istanbul":            "İstanbul",
		"ISTANBUL":            "İstanbul",
		"İSTANBUL":            "İstanbul",
		"  istanbul  ":        "İstanbul",
		"İstanbul":           "İstanbul", // NFD: I + combining dot above
		"izmir":               "İzmir",
		"IZMIR":               "İzmir",
		"sanliurfa":           "Şanlıurfa",
		"urfa":                "Şanlıurfa",
		"canakkale":           "Çanakkale",
		"IĞDIR":               "Iğdır",
		"igdir":               "Iğdır",
		"Isparta":             "Isparta", // dotless-I province must not become İsparta
		"kahramanmaras":       "Kahramanmaraş",
		"Afyon":               "Afyonkarahisar",
		"istanbul   province": "İstanbul",
	}
	for in, want := range cases {
		got, err := NormalizeCity(in)
		if err != nil || got != want {
			t.Errorf("NormalizeCity(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeCityRejects(t *testing.T) {
	for _, in := range []string{
		"", "Atlantis", "London", "İstanbul' OR '1'='1", "İstanbul; DROP TABLE users;--",
		"%", "*", "İstanbul%", string(make([]byte, 65)),
	} {
		if got, err := NormalizeCity(in); err == nil {
			t.Errorf("NormalizeCity(%q) = %q, want error", in, got)
		}
	}
}

func TestProvinceList(t *testing.T) {
	if n := len(Provinces()); n != 81 {
		t.Fatalf("want 81 provinces, got %d", n)
	}
	for _, p := range Provinces() { // every canonical name round-trips
		if got, err := NormalizeCity(p); err != nil || got != p {
			t.Errorf("round-trip %q → %q, %v", p, got, err)
		}
	}
}
