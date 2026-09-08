package diskinstall

import (
	"testing"
)

func TestValidateHostname(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid default", "omahab", false},
		{"valid marisol", "marisol", false},
		{"valid hyphen", "my-host", false},
		{"valid single char", "a", false},
		{"empty", "", true},
		{"too long", string(make([]byte, 64)), true},
		{"uppercase", "MyHost", true},
		{"leading hyphen", "-host", true},
		{"trailing hyphen", "host-", true},
		{"with dot", "host.local", true},
		{"with underscore", "host_name", true},
		{"max63", "a" + string(make([]byte, 62)) + "z", true}, // Actually need 63 chars exactly
	}
	// Correct max63 case: 63 chars alphanumeric
	max63 := "a"
	for i := 0; i < 61; i++ {
		max63 += "b"
	}
	max63 += "c" // total 63
	tests[11] = struct {
		name    string
		in      string
		wantErr bool
	}{"max63 valid", max63, false}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateHostname(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateHostname(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"marisol", false},
		{"omahab", false}, // allowed
		{"_user", false},
		{"user_1", false},
		{"a", false},
		{"", true},
		{"Root", true}, // case-insensitive root
		{"root", true},
		{"OMAHAB-BUILDER", true},
		{"omahab-builder", true},
		{"invalid-!", true},
		{"123user", true}, // must start with letter or underscore
		{"a-very-long-username-exceeding-thirty-two-chars", true},
		{"nobody", true},
		{"sshd", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateUsername(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateUsername(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Fatal("short password should fail")
	}
	if err := ValidatePassword(""); err == nil {
		t.Fatal("empty password should fail")
	}
	if err := ValidatePassword("longenough"); err != nil {
		t.Fatalf("valid password failed: %v", err)
	}
	if err := ValidatePasswordConfirmation("abcd1234", "abcd1234"); err != nil {
		t.Fatalf("matching passwords should pass: %v", err)
	}
	if err := ValidatePasswordConfirmation("abcd1234", "different"); err == nil {
		t.Fatal("mismatched passwords should fail")
	}
}

func TestValidateHostnameTrim(t *testing.T) {
	if err := ValidateHostname(" omahab "); err != nil {
		t.Fatalf("trimmed hostname should pass: %v", err)
	}
}
