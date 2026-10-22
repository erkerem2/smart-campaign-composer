package segment

import (
	"fmt"
	"strings"
	"unicode"
)

// provinces are Türkiye's 81 provinces in their canonical Turkish spelling.
// This is the only set of values the `city` field accepts.
var provinces = []string{
	"Adana", "Adıyaman", "Afyonkarahisar", "Ağrı", "Aksaray", "Amasya", "Ankara",
	"Antalya", "Ardahan", "Artvin", "Aydın", "Balıkesir", "Bartın", "Batman",
	"Bayburt", "Bilecik", "Bingöl", "Bitlis", "Bolu", "Burdur", "Bursa",
	"Çanakkale", "Çankırı", "Çorum", "Denizli", "Diyarbakır", "Düzce", "Edirne",
	"Elazığ", "Erzincan", "Erzurum", "Eskişehir", "Gaziantep", "Giresun",
	"Gümüşhane", "Hakkari", "Hatay", "Iğdır", "Isparta", "İstanbul", "İzmir",
	"Kahramanmaraş", "Karabük", "Karaman", "Kars", "Kastamonu", "Kayseri",
	"Kırıkkale", "Kırklareli", "Kırşehir", "Kilis", "Kocaeli", "Konya", "Kütahya",
	"Malatya", "Manisa", "Mardin", "Mersin", "Muğla", "Muş", "Nevşehir", "Niğde",
	"Ordu", "Osmaniye", "Rize", "Sakarya", "Samsun", "Siirt", "Sinop", "Sivas",
	"Şanlıurfa", "Şırnak", "Tekirdağ", "Tokat", "Trabzon", "Tunceli", "Uşak",
	"Van", "Yalova", "Yozgat", "Zonguldak",
}

// cityAliases maps common colloquial/legacy names (already folded) to the
// canonical province.
var cityAliases = map[string]string{
	"afyon":             "Afyonkarahisar",
	"antep":             "Gaziantep",
	"urfa":              "Şanlıurfa",
	"maras":             "Kahramanmaraş",
	"icel":              "Mersin",
	"izmit":             "Kocaeli",
	"adapazari":         "Sakarya",
	"constantinople":    "İstanbul",
	"istanbul province": "İstanbul",
}

var cityIndex = buildCityIndex()

func buildCityIndex() map[string]string {
	idx := make(map[string]string, len(provinces)+len(cityAliases))
	for _, p := range provinces {
		k := foldCity(p)
		if prev, dup := idx[k]; dup {
			panic(fmt.Sprintf("segment: city fold collision %q vs %q", prev, p))
		}
		idx[k] = p
	}
	for k, v := range cityAliases {
		idx[k] = v
	}
	return idx
}

var turkishFold = strings.NewReplacer(
	"İ", "i", "I", "i", "ı", "i",
	"Ş", "s", "ş", "s",
	"Ğ", "g", "ğ", "g",
	"Ü", "u", "ü", "u",
	"Ö", "o", "ö", "o",
	"Ç", "c", "ç", "c",
	"Â", "a", "â", "a",
	"Î", "i", "î", "i",
	"Û", "u", "û", "u",
)

// foldCity produces a case- and diacritic-insensitive key:
// "İSTANBUL", "Istanbul", "istanbul", "i̇stanbul" → "istanbul".
// Turkish dotted/dotless I are folded explicitly before Unicode lowering,
// because Go's default case mapping is not Turkish-aware.
func foldCity(s string) string {
	s = turkishFold.Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.Is(unicode.Mn, r): // combining marks (e.g. U+0307 from NFD input)
			continue
		case unicode.IsSpace(r) || r == '-' || r == '_':
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return turkishFold.Replace(b.String())
}

// NormalizeCity resolves any spelling of a Turkish province to its canonical
// name. Use it on BOTH paths: when ingesting/seeding user data (normalise on
// write, so the DB column holds canonical names and stays index-friendly) and
// when validating rule values (normalise on read).
func NormalizeCity(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > maxStringLen {
		return "", fmt.Errorf("city length out of range")
	}
	if c, ok := cityIndex[foldCity(raw)]; ok {
		return c, nil
	}
	return "", fmt.Errorf("unknown city %q", raw)
}

// Provinces returns a copy of the canonical province list.
func Provinces() []string { return append([]string(nil), provinces...) }
