package session

import "testing"

type payload struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles,omitempty"`
}

func TestJSONCodec_Roundtrip(t *testing.T) {
	c := JSONCodec[payload]{}
	in := payload{Name: "alice", Roles: []string{"admin"}}

	b, err := c.Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || len(out.Roles) != 1 || out.Roles[0] != in.Roles[0] {
		t.Errorf("roundtrip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestJSONCodec_ZeroValueRoundtrip(t *testing.T) {
	c := JSONCodec[payload]{}
	b, err := c.Encode(payload{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "" || len(out.Roles) != 0 {
		t.Errorf("zero roundtrip: got %+v", out)
	}
}
