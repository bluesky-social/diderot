package slack

import "testing"

func TestImageRefsFromAttachments(t *testing.T) {
	tests := []struct {
		name string
		in   []FileAttachment
		want int // expected len; per-element fields checked separately for the multi case
	}{
		{name: "nil", in: nil, want: 0},
		{name: "empty", in: []FileAttachment{}, want: 0},
		{
			name: "single",
			in: []FileAttachment{
				{FileID: "F1", Mimetype: "image/png"},
			},
			want: 1,
		},
		{
			name: "filters unsupported and non-images",
			in: []FileAttachment{
				{FileID: "F1", Mimetype: "image/png"},
				{FileID: "F2", Mimetype: "image/jpeg"},
				{FileID: "F3", Mimetype: ""},
				{FileID: "F4", Mimetype: "application/pdf"},
				{FileID: "F5", Mimetype: "image/gif"},
			},
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ImageRefsFromAttachments(tc.in)
			if len(got) != tc.want {
				t.Fatalf("len: got %d, want %d", len(got), tc.want)
			}
			for i, r := range got {
				if r.SourceImageID != tc.in[i].FileID {
					t.Errorf("[%d] SourceImageID: got %q, want %q",
						i, r.SourceImageID, tc.in[i].FileID)
				}
				if r.Mimetype != tc.in[i].Mimetype {
					t.Errorf("[%d] Mimetype: got %q, want %q",
						i, r.Mimetype, tc.in[i].Mimetype)
				}
			}
		})
	}
}
