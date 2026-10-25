package camera

import "testing"

func TestParsePresetToken(t *testing.T) {
	tests := []struct {
		name string
		xml  string
		want string
	}{
		{
			name: "tptz prefix",
			xml: `<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope" xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl">
  <soap:Body>
    <tptz:SetPresetResponse>
      <tptz:PresetToken>3</tptz:PresetToken>
    </tptz:SetPresetResponse>
  </soap:Body>
</soap:Envelope>`,
			want: "3",
		},
		{
			name: "no namespace prefix",
			xml: `<SetPresetResponse>
  <PresetToken>Preset_4</PresetToken>
</SetPresetResponse>`,
			want: "Preset_4",
		},
		{
			name: "empty or missing",
			xml:  `<SetPresetResponse></SetPresetResponse>`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePresetToken(tt.xml)
			if got != tt.want {
				t.Errorf("parsePresetToken() = %q, want %q", got, tt.want)
			}
		})
	}
}
