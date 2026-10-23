package siprecsrv

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

// Media types are case-insensitive (RFC 2045 §5.1) and take parameters;
// a type that merely starts like SDP or RS-Metadata is neither.
func TestBodyPartTypesMatchCaseInsensitively(t *testing.T) {
	sdp := string(covSDP("40000 RTP/AVP 0"))
	meta := modSiprecMetadataXML("123e4567-e89b-12d3-a456-426614174000")
	const ct = `multipart/mixed; boundary="b"`

	body := covMultipart("b", [2]string{"Application/SDP", sdp},
		[2]string{"APPLICATION/RS-METADATA+XML; charset=UTF-8", meta})

	gotSDP, gotMeta, err := extractSDPAndMetadata(covRequest(sip.INVITE, "c", covFrom, "", body, ct))
	if err != nil || gotSDP != sdp || gotMeta != meta {
		t.Errorf("mixed-case parts: sdp %v metadata %v, %v", gotSDP == sdp, gotMeta == meta, err)
	}

	bare := covMultipart("b", [2]string{"application/sdp", sdp}, [2]string{"application/rs-metadata", meta})
	if _, gotMeta, err := extractSDPAndMetadata(covRequest(sip.INVITE, "c", covFrom, "", bare, ct)); err != nil || gotMeta != meta {
		t.Errorf("bare application/rs-metadata part not read as metadata: %v", err)
	}

	lookalikes := covMultipart("b", [2]string{"application/sdpx", sdp},
		[2]string{"application/rs-metadata-request", meta}, [2]string{"", sdp})
	if _, gotMeta, err := extractSDPAndMetadata(covRequest(sip.INVITE, "c", covFrom, "", lookalikes, ct)); err == nil || gotMeta != "" {
		t.Errorf("look-alike part types were read: metadata %q, err %v", gotMeta, err)
	}

	// Only a broken parameter: the media type still names the part.
	brokenParam := covMultipart("b", [2]string{"application/sdp; =broken", sdp})
	if gotSDP, _, err := extractSDPAndMetadata(covRequest(sip.INVITE, "c", covFrom, "", brokenParam, ct)); err != nil || gotSDP != sdp {
		t.Errorf("SDP part with a broken parameter: %v", err)
	}

	// A whole body of RS-Metadata (an UPDATE), any case.
	gotSDP, gotMeta, multi, err := splitBody(covRequest(sip.UPDATE, "c", covFrom, "", []byte(meta), "Application/RS-Metadata+XML"))
	if err != nil || multi || gotSDP != "" || gotMeta != meta {
		t.Errorf("single metadata body: sdp %q multi %v metadata %v, %v", gotSDP, multi, gotMeta == meta, err)
	}
}
