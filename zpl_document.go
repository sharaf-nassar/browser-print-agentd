package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	cupsFilterPath = "/usr/sbin/cupsfilter"
	cupsPPDDir     = "/etc/cups/ppd"

	maxQueueNameBytes      = 127
	maxQueuePPDBytes       = 2 << 20
	maxFilterOutputBytes   = 8 << 20
	maxFilterErrorBytes    = 1 << 20
	maxDecodedBitmapBytes  = 4 << 20
	maxRasterDimensionDots = 10000
	maxRasterPages         = 16
	maxGraphicFieldBytes   = 99999
)

// documentRenderer turns a PDF into the printer language emitted by the
// destination's real PPD. Production uses cupsFilterRenderer; tests inject a
// renderer that returns captured rastertolabel output without invoking CUPS.
type documentRenderer interface {
	Render(ctx context.Context, queue string, document []byte) ([]byte, error)
}

type cupsFilterRenderer struct {
	runner execRunner
}

// Render runs the stock macOS filter chain without submitting a CUPS job.
// The queue PPD is copied after validation so the conversion cannot race a
// queue reconfiguration between validation and cupsfilter opening the file.
func (r cupsFilterRenderer) Render(
	ctx context.Context, queue string, document []byte,
) ([]byte, error) {
	ppdPath, err := queuePPDPath(queue)
	if err != nil {
		return nil, err
	}
	ppd, err := readBoundedFile(ppdPath, maxQueuePPDBytes)
	if err != nil {
		return nil, fmt.Errorf("read queue PPD: %w", err)
	}
	if err := validateRasterToLabelPPD(ppd); err != nil {
		return nil, fmt.Errorf("validate queue PPD: %w", err)
	}

	dir, err := os.MkdirTemp("", tempPrefix+"-render-*")
	if err != nil {
		return nil, fmt.Errorf("create render directory: %w", err)
	}
	defer os.RemoveAll(dir)

	stablePPD := filepath.Join(dir, "queue.ppd")
	inputPDF := filepath.Join(dir, "document.pdf")
	if err := os.WriteFile(stablePPD, ppd, 0o600); err != nil {
		return nil, fmt.Errorf("write render PPD: %w", err)
	}
	if err := os.WriteFile(inputPDF, document, 0o600); err != nil {
		return nil, fmt.Errorf("write render PDF: %w", err)
	}

	result, err := r.runner.Run(ctx, cupsFilterPath,
		"-p", stablePPD,
		"-m", "printer/foo",
		"-e",
		"-o", reversePortrait,
		inputPDF,
	)
	if err != nil {
		return nil, fmt.Errorf("render PDF with cupsfilter: %w", err)
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = "cupsfilter failed"
		}
		return nil, fmt.Errorf("render PDF with cupsfilter: %s", detail)
	}
	if len(result.Stdout) == 0 {
		return nil, fmt.Errorf("render PDF with cupsfilter: empty output")
	}
	return []byte(result.Stdout), nil
}

func queuePPDPath(queue string) (string, error) {
	if queue == "" || len(queue) > maxQueueNameBytes || queue == "." || queue == ".." ||
		strings.ContainsAny(queue, " /#\t\r\n") || filepath.Base(queue) != queue {
		return "", fmt.Errorf("invalid CUPS queue name %q", queue)
	}
	return filepath.Join(cupsPPDDir, queue+".ppd"), nil
}

func readBoundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func validateRasterToLabelPPD(ppd []byte) error {
	required := []string{
		`*NickName: "Zebra ZPL Label Printer"`,
		`*cupsModelNumber: 18`,
		`*cupsFilter: "application/vnd.cups-raster 50 rastertolabel"`,
	}
	text := string(ppd)
	for _, line := range required {
		if !hasExactLine(text, line) {
			return fmt.Errorf("missing required directive %q", line)
		}
	}
	return nil
}

func hasExactLine(text string, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSuffix(line, "\r") == want {
			return true
		}
	}
	return false
}

type rasterLabelPage struct {
	darkness  string
	width     int
	height    int
	rowBytes  int
	media     string
	printRate string
	settings  []string
	cut       bool
	bitmap    []byte
}

// transformRasterLabelZPL accepts only the straight-line ZPL grammar emitted
// by Apple's ZEBRA_ZPL rastertolabel arm. It never forwards arbitrary filter
// output to a printer: every accepted command is parsed below, stored graphics
// are decoded to bounded pixels, and a new command stream is authored here.
func transformRasterLabelZPL(input []byte) ([]byte, error) {
	if len(input) == 0 || len(input) > maxFilterOutputBytes {
		return nil, fmt.Errorf("rastertolabel output size %d is outside 1..%d bytes",
			len(input), maxFilterOutputBytes)
	}
	if bytes.IndexByte(input, 0) >= 0 {
		return nil, fmt.Errorf("rastertolabel output contains a NUL byte")
	}

	text := string(input)
	offset := 0
	pages := make([]rasterLabelPage, 0, 1)
	decodedBytes := 0
	for offset < len(text) {
		if len(pages) == maxRasterPages {
			return nil, fmt.Errorf("rastertolabel output exceeds %d pages", maxRasterPages)
		}
		page, next, err := parseRasterLabelPage(text, offset)
		if err != nil {
			return nil, fmt.Errorf("parse rastertolabel page %d: %w", len(pages)+1, err)
		}
		decodedBytes += len(page.bitmap)
		if decodedBytes > maxDecodedBitmapBytes {
			return nil, fmt.Errorf("decoded raster data exceeds %d bytes", maxDecodedBitmapBytes)
		}
		pages = append(pages, page)
		offset = next
	}

	var output bytes.Buffer
	for index, page := range pages {
		if err := appendInlineRasterPage(&output, page); err != nil {
			return nil, fmt.Errorf("encode rastertolabel page %d: %w", index+1, err)
		}
		if output.Len() > maxWriteBody {
			return nil, fmt.Errorf("transformed ZPL exceeds %d bytes", maxWriteBody)
		}
	}
	return output.Bytes(), nil
}

func parseRasterLabelPage(text string, offset int) (rasterLabelPage, int, error) {
	page := rasterLabelPage{}
	if strings.HasPrefix(text[offset:], "~SD") {
		line, next, err := takeZPLLine(text, offset)
		if err != nil || !validUnsignedCommand(line, "~SD", 0, 30) {
			return page, offset, fmt.Errorf("invalid darkness command")
		}
		page.darkness = line
		offset = next
	}

	header, next, err := takeZPLLine(text, offset)
	if err != nil {
		return page, offset, fmt.Errorf("read download header: %w", err)
	}
	total, rowBytes, err := parseDownloadHeader(header)
	if err != nil {
		return page, offset, err
	}
	dataStart := next
	marker := strings.Index(text[dataStart:], "^XA\n")
	if marker < 0 {
		return page, offset, fmt.Errorf("missing format start after graphic data")
	}
	encoded := text[dataStart : dataStart+marker]
	bitmap, err := decodeClassicZPLGraphic(encoded, total, rowBytes)
	if err != nil {
		return page, offset, err
	}
	page.bitmap = bitmap
	page.rowBytes = rowBytes
	page.height = total / rowBytes
	offset = dataStart + marker + len("^XA\n")

	line, offset, err := takeZPLLine(text, offset)
	if err != nil || line != "^POI" {
		return page, offset, fmt.Errorf("expected ^POI")
	}
	line, offset, err = takeZPLLine(text, offset)
	if err != nil || !strings.HasPrefix(line, "^PW") {
		return page, offset, fmt.Errorf("expected ^PW")
	}
	page.width, err = parseBoundedUnsigned(strings.TrimPrefix(line, "^PW"), 1,
		maxRasterDimensionDots)
	if err != nil || page.rowBytes != (page.width+7)/8 ||
		page.height < 1 || page.height > maxRasterDimensionDots {
		return page, offset, fmt.Errorf("invalid raster dimensions %dx%d with %d bytes per row",
			page.width, page.height, page.rowBytes)
	}

	line, next, err = takeZPLLine(text, offset)
	if err != nil {
		return page, offset, err
	}
	if strings.HasPrefix(line, "^PR") {
		if !validPrintRate(line) {
			return page, offset, fmt.Errorf("invalid print-rate command")
		}
		page.printRate = line
		offset = next
		line, next, err = takeZPLLine(text, offset)
		if err != nil {
			return page, offset, err
		}
	}
	if line != "^LH0,0" {
		return page, offset, fmt.Errorf("expected ^LH0,0")
	}
	offset = next

	line, next, err = takeZPLLine(text, offset)
	if err != nil {
		return page, offset, err
	}
	if strings.HasPrefix(line, "^LL") {
		length, parseErr := parseBoundedUnsigned(strings.TrimPrefix(line, "^LL"), 1,
			maxRasterDimensionDots)
		if parseErr != nil || length != page.height {
			return page, offset, fmt.Errorf("invalid label length %q", line)
		}
		offset = next
		line, next, err = takeZPLLine(text, offset)
		if err != nil {
			return page, offset, err
		}
	}
	if line != "^MNN" && line != "^MNY" && line != "^MNM" {
		return page, offset, fmt.Errorf("invalid media-tracking command %q", line)
	}
	page.media = line
	offset = next

	validators := []func(string) bool{
		func(value string) bool { return validSignedCommand(value, "^LT", -10000, 10000) },
		func(value string) bool { return value == "^MTT" || value == "^MTD" },
		validPrintMode,
		func(value string) bool { return validSignedCommand(value, "~TA", -10000, 10000) },
		func(value string) bool { return value == "^JZY" || value == "^JZN" },
		validPrintQuantity,
	}
	for _, valid := range validators {
		line, next, err = takeZPLLine(text, offset)
		if err != nil {
			return page, offset, err
		}
		if valid(line) {
			page.settings = append(page.settings, line)
			offset = next
		}
	}

	expected := []string{
		"^FO0,0^XGR:CUPS.GRF,1,1^FS",
		"^XZ",
		"^XA",
		"^IDR:CUPS.GRF^FS",
		"^XZ",
	}
	for _, want := range expected {
		line, next, err = takeZPLLine(text, offset)
		if err != nil || line != want {
			return page, offset, fmt.Errorf("expected %q", want)
		}
		offset = next
	}
	if offset < len(text) && strings.HasPrefix(text[offset:], "^CN1\n") {
		page.cut = true
		offset += len("^CN1\n")
	}
	return page, offset, nil
}

func parseDownloadHeader(line string) (int, int, error) {
	parts := strings.Split(line, ",")
	if len(parts) != 4 || parts[0] != "~DGR:CUPS.GRF" || parts[3] != "" {
		return 0, 0, fmt.Errorf("invalid graphic download header %q", line)
	}
	total, err := parseBoundedUnsigned(parts[1], 1, maxDecodedBitmapBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid graphic byte count")
	}
	rowBytes, err := parseBoundedUnsigned(parts[2], 1, maxGraphicFieldBytes)
	if err != nil || total%rowBytes != 0 {
		return 0, 0, fmt.Errorf("invalid graphic row byte count")
	}
	return total, rowBytes, nil
}

func decodeClassicZPLGraphic(encoded string, total int, rowBytes int) ([]byte, error) {
	rowHexBytes := rowBytes * 2
	raw := make([]byte, 0, total)
	row := make([]byte, 0, rowHexBytes)
	var previous []byte

	finishRow := func(fill byte) error {
		if len(row) > rowHexBytes {
			return fmt.Errorf("graphic row exceeds %d bytes", rowBytes)
		}
		for len(row) < rowHexBytes {
			row = append(row, fill)
		}
		if len(raw)+rowBytes > total {
			return fmt.Errorf("graphic data exceeds declared %d bytes", total)
		}
		decoded := make([]byte, rowBytes)
		for index := range decoded {
			high, okHigh := zplHexNibble(row[index*2])
			low, okLow := zplHexNibble(row[index*2+1])
			if !okHigh || !okLow {
				return fmt.Errorf("graphic row contains non-hex data")
			}
			decoded[index] = high<<4 | low
		}
		raw = append(raw, decoded...)
		previous = decoded
		row = row[:0]
		return nil
	}

	for index := 0; index < len(encoded); {
		character := encoded[index]
		switch character {
		case ':':
			if len(row) != 0 || previous == nil || len(raw)+rowBytes > total {
				return nil, fmt.Errorf("invalid repeated graphic row")
			}
			raw = append(raw, previous...)
			index++
			continue
		case ',', '!':
			fill := byte('0')
			if character == '!' {
				fill = 'F'
			}
			if err := finishRow(fill); err != nil {
				return nil, err
			}
			index++
			continue
		}

		repeat := 0
		for index < len(encoded) {
			character = encoded[index]
			switch {
			case character >= 'G' && character <= 'Y':
				repeat += int(character-'G') + 1
			case character >= 'g' && character <= 'z':
				repeat += (int(character-'g') + 1) * 20
			default:
				goto countDone
			}
			if repeat > rowHexBytes {
				return nil, fmt.Errorf("graphic run exceeds row width")
			}
			index++
		}
	countDone:
		if index >= len(encoded) {
			return nil, fmt.Errorf("graphic run has no hex value")
		}
		character = encoded[index]
		if _, ok := zplHexNibble(character); !ok {
			return nil, fmt.Errorf("unexpected graphic byte %q", character)
		}
		if repeat == 0 {
			repeat = 1
		}
		if len(row)+repeat > rowHexBytes {
			return nil, fmt.Errorf("graphic row exceeds %d bytes", rowBytes)
		}
		for count := 0; count < repeat; count++ {
			row = append(row, character)
		}
		index++
		if len(row) == rowHexBytes {
			if err := finishRow('0'); err != nil {
				return nil, err
			}
		}
	}
	if len(row) != 0 || len(raw) != total {
		return nil, fmt.Errorf("graphic decoded to %d of %d bytes", len(raw), total)
	}
	return raw, nil
}

func appendInlineRasterPage(output *bytes.Buffer, page rasterLabelPage) error {
	rotated, err := rotateBitmap180(page.bitmap, page.width, page.height, page.rowBytes)
	if err != nil {
		return err
	}
	writeZPLLine(output, page.darkness)
	writeZPLLine(output, "^XA")
	writeZPLLine(output, "^PON")
	writeZPLLine(output, fmt.Sprintf("^PW%d", page.width))
	writeZPLLine(output, page.printRate)
	writeZPLLine(output, "^LH0,0")
	writeZPLLine(output, fmt.Sprintf("^LL%d", page.height))
	writeZPLLine(output, page.media)
	for _, setting := range page.settings {
		writeZPLLine(output, setting)
	}

	rowsPerBand := maxGraphicFieldBytes / page.rowBytes
	if rowsPerBand < 1 {
		return fmt.Errorf("row width %d exceeds ^GF limit", page.rowBytes)
	}
	for firstRow := 0; firstRow < page.height; firstRow += rowsPerBand {
		rows := rowsPerBand
		if remaining := page.height - firstRow; remaining < rows {
			rows = remaining
		}
		band := rotated[firstRow*page.rowBytes : (firstRow+rows)*page.rowBytes]
		fmt.Fprintf(output, "^FO0,%d^GFA,%d,%d,%d,", firstRow, len(band), len(band),
			page.rowBytes)
		writeUpperHex(output, band)
		output.WriteString("^FS\n")
	}
	writeZPLLine(output, "^XZ")
	if page.cut {
		writeZPLLine(output, "^CN1")
	}
	return nil
}

func rotateBitmap180(bitmap []byte, width int, height int, rowBytes int) ([]byte, error) {
	if len(bitmap) != rowBytes*height {
		return nil, fmt.Errorf("bitmap dimensions do not match payload")
	}
	if remainder := width % 8; remainder != 0 {
		paddingMask := byte(1<<(8-remainder)) - 1
		for row := 0; row < height; row++ {
			if bitmap[(row+1)*rowBytes-1]&paddingMask != 0 {
				return nil, fmt.Errorf("bitmap has nonzero pixels outside ^PW")
			}
		}
	}
	rotated := make([]byte, len(bitmap))
	for sourceY := 0; sourceY < height; sourceY++ {
		for sourceX := 0; sourceX < width; sourceX++ {
			sourceByte := sourceY*rowBytes + sourceX/8
			if bitmap[sourceByte]&(0x80>>uint(sourceX%8)) == 0 {
				continue
			}
			targetX := width - 1 - sourceX
			targetY := height - 1 - sourceY
			targetByte := targetY*rowBytes + targetX/8
			rotated[targetByte] |= 0x80 >> uint(targetX%8)
		}
	}
	return rotated, nil
}

func takeZPLLine(text string, offset int) (string, int, error) {
	if offset >= len(text) {
		return "", offset, fmt.Errorf("unexpected end of output")
	}
	end := strings.IndexByte(text[offset:], '\n')
	if end < 0 {
		return "", offset, fmt.Errorf("unterminated command")
	}
	line := text[offset : offset+end]
	if strings.ContainsRune(line, '\r') {
		return "", offset, fmt.Errorf("command contains carriage return")
	}
	return line, offset + end + 1, nil
}

func parseBoundedUnsigned(value string, minimum int, maximum int) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("invalid unsigned number %q", value)
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("number %q outside %d..%d", value, minimum, maximum)
	}
	return parsed, nil
}

func validUnsignedCommand(line string, prefix string, minimum int, maximum int) bool {
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	_, err := parseBoundedUnsigned(strings.TrimPrefix(line, prefix), minimum, maximum)
	return err == nil
}

func validSignedCommand(line string, prefix string, minimum int, maximum int) bool {
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	value := strings.TrimPrefix(line, prefix)
	if value == "" {
		return false
	}
	if value[0] == '-' || value[0] == '+' {
		value = value[1:]
	}
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	parsed, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
	return err == nil && parsed >= minimum && parsed <= maximum
}

func validPrintRate(line string) bool {
	if !strings.HasPrefix(line, "^PR") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(line, "^PR"), ",")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if _, err := parseBoundedUnsigned(part, 1, 99); err != nil {
			return false
		}
	}
	return true
}

func validPrintMode(line string) bool {
	if len(line) != len("^MMT,Y") || !strings.HasPrefix(line, "^MM") ||
		line[4:] != ",Y" {
		return false
	}
	return strings.ContainsRune("TPRAC", rune(line[3]))
}

func validPrintQuantity(line string) bool {
	if !strings.HasPrefix(line, "^PQ") || !strings.HasSuffix(line, ", 0, 0, N") {
		return false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(line, "^PQ"), ", 0, 0, N")
	_, err := parseBoundedUnsigned(value, 2, 99999999)
	return err == nil
}

func zplHexNibble(character byte) (byte, bool) {
	switch {
	case character >= '0' && character <= '9':
		return character - '0', true
	case character >= 'A' && character <= 'F':
		return character - 'A' + 10, true
	default:
		return 0, false
	}
}

func writeZPLLine(output *bytes.Buffer, line string) {
	if line == "" {
		return
	}
	output.WriteString(line)
	output.WriteByte('\n')
}

func writeUpperHex(output *bytes.Buffer, data []byte) {
	const digits = "0123456789ABCDEF"
	for _, value := range data {
		output.WriteByte(digits[value>>4])
		output.WriteByte(digits[value&15])
	}
}

func renderTimeout(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return cupsPrintTimeout
}
