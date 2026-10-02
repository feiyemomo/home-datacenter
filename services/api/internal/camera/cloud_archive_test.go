package camera

import (
	"encoding/xml"
	"testing"
)

func TestWebdavPropfindXmlParsing(t *testing.T) {
	rawXML := `<?xml version="1.0" encoding="utf-8" ?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/dav/quark/Surveillance/Recordings/2026-09-27/03/front_door/</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>front_door</D:displayname>
        <D:resourcetype><D:collection/></D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/quark/Surveillance/Recordings/2026-09-27/03/front_door/17.34.mp4</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>17.34.mp4</D:displayname>
        <D:resourcetype></D:resourcetype>
        <D:getcontentlength>3785589</D:getcontentlength>
        <D:getcontenttype>video/mp4</D:getcontenttype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`

	var parsed propfindResponse
	err := xml.Unmarshal([]byte(rawXML), &parsed)
	if err != nil {
		t.Fatalf("failed to unmarshal propfind xml: %v", err)
	}

	if len(parsed.Responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(parsed.Responses))
	}

	dirResp := parsed.Responses[0]
	if dirResp.Propstat.Prop.ResourceType.Collection == nil {
		t.Errorf("expected first item to be collection")
	}

	fileResp := parsed.Responses[1]
	if fileResp.Propstat.Prop.DisplayName != "17.34.mp4" {
		t.Errorf("expected 17.34.mp4, got %q", fileResp.Propstat.Prop.DisplayName)
	}
	if fileResp.Propstat.Prop.ContentLength != 3785589 {
		t.Errorf("expected content length 3785589, got %d", fileResp.Propstat.Prop.ContentLength)
	}
	if fileResp.Propstat.Prop.ResourceType.Collection != nil {
		t.Errorf("expected file not to be collection")
	}
}
