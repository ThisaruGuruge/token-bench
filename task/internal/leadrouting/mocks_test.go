package leadrouting

import "testing"

func TestRatingIgnoresTheInstructionAroundTheMessage(t *testing.T) {
	instruction := "Classify the lead as HOT (strong buying intent, asks for a quote or pricing) or COLD. Answer with one word.\\n\\nMessage: "
	cases := map[string]string{
		instruction + ColdMessage: "COLD",
		instruction + HotMessage:  "HOT",
		ColdMessage:               "COLD",
		HotMessage:                "HOT",
		"Please send a quote":     "HOT",
		"Hello there":             "COLD",
	}
	for text, want := range cases {
		if got := Rating(text); got != want {
			t.Errorf("Rating(%q) = %s, want %s", text, got, want)
		}
	}
}
