package eval

import "testing"

func TestProblemsLoad(t *testing.T) {
	if len(Problems) != 200 || Problems[0].Answer != "18" {
		t.Fatalf("got %d problems, first answer %q", len(Problems), Problems[0].Answer)
	}
}

func TestCorrect(t *testing.T) {
	cases := []struct {
		reply, expected string
		want            bool
	}{
		{"9 eggs, 9 * 2 = 18.\nAnswer: 18", "18", true},
		{"Answer: $1,250", "1250", true},
		{"Answer: **42**", "42", true},
		{"Answer: 3.50", "3.5", true},
		{"So she makes 18 dollars.", "18", true},
		{"Answer: 17", "18", false},
		{"First 18, then Answer: 20", "18", false},
		{"I don't know.", "18", false},
	}
	for _, c := range cases {
		if got := Correct(c.reply, c.expected); got != c.want {
			t.Errorf("Correct(%q, %q) = %v, want %v", c.reply, c.expected, got, c.want)
		}
	}
}
