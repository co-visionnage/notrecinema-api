package upload

import "bytes"

// SniffedImage — распознанный по магическим байтам тип изображения.
type SniffedImage struct {
	Extension string
	MimeType  string
}

// SniffImageType — прямой перенос sniffImageType из notrecinema-app:
// проверяет реальные байты файла, а не Content-Type/расширение из запроса
// (оба полностью подделываемы клиентом). Намеренно не поддерживает
// image/svg+xml -- SVG может нести <script> и отдавался бы обратно
// публично с этим content-type.
func SniffImageType(data []byte) (SniffedImage, bool) {
	switch {
	case isPNG(data):
		return SniffedImage{Extension: "png", MimeType: "image/png"}, true
	case isJPEG(data):
		return SniffedImage{Extension: "jpg", MimeType: "image/jpeg"}, true
	case isGIF(data):
		return SniffedImage{Extension: "gif", MimeType: "image/gif"}, true
	case isWebP(data):
		return SniffedImage{Extension: "webp", MimeType: "image/webp"}, true
	default:
		return SniffedImage{}, false
	}
}

func isPNG(b []byte) bool {
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	return len(b) >= len(sig) && bytes.Equal(b[:len(sig)], sig)
}

func isJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

func isGIF(b []byte) bool {
	if len(b) < 6 {
		return false
	}
	head := string(b[:6])
	return head == "GIF87a" || head == "GIF89a"
}

func isWebP(b []byte) bool {
	return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}
