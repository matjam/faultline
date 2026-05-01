package skills

import "testing"

func TestValidateName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"pdf-processing", true},
		{"data-analysis", true},
		{"a", true},
		{"abc123", true},
		{"a-1", true},

		{"", false},
		{"PDF-Processing", false}, // uppercase
		{"-pdf", false},           // leading hyphen
		{"pdf-", false},           // trailing hyphen
		{"pdf--processing", false},
		{"pdf processing", false}, // space
		{"pdf_processing", false}, // underscore
		// 65 characters
		{"abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghij-abcdefghij1", false},
	}
	for _, c := range cases {
		err := ValidateName(c.name)
		if c.ok && err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", c.name)
		}
	}
}

func TestValidateDescription(t *testing.T) {
	if err := ValidateDescription(""); err == nil {
		t.Error("ValidateDescription(empty) = nil, want error")
	}
	if err := ValidateDescription("a useful description"); err != nil {
		t.Errorf("ValidateDescription(short) = %v, want nil", err)
	}

	long := make([]byte, MaxDescriptionLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateDescription(string(long)); err == nil {
		t.Error("ValidateDescription(over-long) = nil, want error")
	}
}
