// Copyright 2016 - 2024 The excelize Authors. All rights reserved. Use of
// this source code is governed by a BSD-style license that can be found in
// the LICENSE file.
//
// Package excelize providing a set of functions that allow you to write to and
// read from XLAM / XLSM / XLSX / XLTM / XLTX files. Supports reading and
// writing spreadsheet documents generated by Microsoft Excel™ 2007 and later.
// Supports complex components by high compatibility, and provided streaming
// API for generating or reading data from a worksheet with huge amounts of
// data. This library needs Go version 1.18 or later.

package excelize

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// StreamWriter defined the type of stream writer.
type StreamWriter struct {
	file            *File
	Sheet           string
	SheetID         int
	sheetWritten    bool
	cols            strings.Builder
	worksheet       *xlsxWorksheet
	rawData         bufferedWriter
	rows            int
	mergeCellsCount int
	mergeCells      strings.Builder
	tableParts      string
}

// NewStreamWriter returns stream writer struct by given worksheet name used for
// writing data on a new existing empty worksheet with large amounts of data.
// Note that after writing data with the stream writer for the worksheet, you
// must call the 'Flush' method to end the streaming writing process, ensure
// that the order of row numbers is ascending when set rows, and the normal
// mode functions and stream mode functions can not be work mixed to writing
// data on the worksheets. The stream writer will try to use temporary files on
// disk to reduce the memory usage when in-memory chunks data over 16MB, and
// you can't get cell value at this time. For example, set data for worksheet
// of size 102400 rows x 50 columns with numbers and style:
//
//	f := excelize.NewFile()
//	defer func() {
//	    if err := f.Close(); err != nil {
//	        fmt.Println(err)
//	    }
//	}()
//	sw, err := f.NewStreamWriter("Sheet1")
//	if err != nil {
//	    fmt.Println(err)
//	    return
//	}
//	styleID, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Color: "777777"}})
//	if err != nil {
//	    fmt.Println(err)
//	    return
//	}
//	if err := sw.SetRow("A1",
//	    []interface{}{
//	        excelize.Cell{StyleID: styleID, Value: "Data"},
//	        []excelize.RichTextRun{
//	            {Text: "Rich ", Font: &excelize.Font{Color: "2354e8"}},
//	            {Text: "Text", Font: &excelize.Font{Color: "e83723"}},
//	        },
//	    },
//	    excelize.RowOpts{Height: 45, Hidden: false}); err != nil {
//	    fmt.Println(err)
//	    return
//	}
//	for rowID := 2; rowID <= 102400; rowID++ {
//	    row := make([]interface{}, 50)
//	    for colID := 0; colID < 50; colID++ {
//	        row[colID] = rand.Intn(640000)
//	    }
//	    cell, err := excelize.CoordinatesToCellName(1, rowID)
//	    if err != nil {
//	        fmt.Println(err)
//	        break
//	    }
//	    if err := sw.SetRow(cell, row); err != nil {
//	        fmt.Println(err)
//	        break
//	    }
//	}
//	if err := sw.Flush(); err != nil {
//	    fmt.Println(err)
//	    return
//	}
//	if err := f.SaveAs("Book1.xlsx"); err != nil {
//	    fmt.Println(err)
//	}
//
// Set cell value and cell formula for a worksheet with stream writer:
//
//	err := sw.SetRow("A1", []interface{}{
//	    excelize.Cell{Value: 1},
//	    excelize.Cell{Value: 2},
//	    excelize.Cell{Formula: "SUM(A1,B1)"}});
//
// Set cell value and rows style for a worksheet with stream writer:
//
//	err := sw.SetRow("A1", []interface{}{
//	    excelize.Cell{Value: 1}},
//	    excelize.RowOpts{StyleID: styleID, Height: 20, Hidden: false});
func (f *File) NewStreamWriter(sheet string) (*StreamWriter, error) {
	if err := checkSheetName(sheet); err != nil {
		return nil, err
	}
	sheetID := f.getSheetID(sheet)
	if sheetID == -1 {
		return nil, ErrSheetNotExist{sheet}
	}
	sw := &StreamWriter{
		file:    f,
		Sheet:   sheet,
		SheetID: sheetID,
	}
	var err error
	sw.worksheet, err = f.workSheetReader(sheet)
	if err != nil {
		return nil, err
	}

	sheetXMLPath, _ := f.getSheetXMLPath(sheet)
	if f.streams == nil {
		f.streams = make(map[string]*StreamWriter)
	}
	f.streams[sheetXMLPath] = sw

	_, _ = sw.rawData.WriteString(xml.Header + `<worksheet` + templateNamespaceIDMap)
	bulkAppendFields(&sw.rawData, sw.worksheet, 2, 3)
	return sw, err
}

// AddTable creates an Excel table for the StreamWriter using the given
// cell range and format set. For example, create a table of A1:D5:
//
//	err := sw.AddTable(&excelize.Table{Range: "A1:D5"})
//
// Create a table of F2:H6 with format set:
//
//	disable := false
//	err := sw.AddTable(&excelize.Table{
//	    Range:             "F2:H6",
//	    Name:              "table",
//	    StyleName:         "TableStyleMedium2",
//	    ShowFirstColumn:   true,
//	    ShowLastColumn:    true,
//	    ShowRowStripes:    &disable,
//	    ShowColumnStripes: true,
//	})
//
// Note that the table must be at least two lines including the header. The
// header cells must contain strings and must be unique.
//
// Currently, only one table is allowed for a StreamWriter. AddTable must be
// called after the rows are written but before Flush.
//
// See File.AddTable for details on the table format.
func (sw *StreamWriter) AddTable(table *Table) error {
	options, err := parseTableOptions(table)
	if err != nil {
		return err
	}
	coordinates, err := rangeRefToCoordinates(options.Range)
	if err != nil {
		return err
	}
	_ = sortCoordinates(coordinates)

	// Correct the minimum number of rows, the table at least two lines.
	if coordinates[1] == coordinates[3] {
		coordinates[3]++
	}

	// Correct table reference range, such correct C1:B3 to B1:C3.
	ref, err := coordinatesToRangeRef(coordinates)
	if err != nil {
		return err
	}

	// create table columns using the first row
	tableHeaders, err := sw.getRowValues(coordinates[1], coordinates[0], coordinates[2])
	if err != nil {
		return err
	}
	tableColumn := make([]*xlsxTableColumn, len(tableHeaders))
	for i, name := range tableHeaders {
		tableColumn[i] = &xlsxTableColumn{
			ID:   i + 1,
			Name: name,
		}
	}

	tableID := sw.file.countTables() + 1

	name := options.Name
	if name == "" {
		name = "Table" + strconv.Itoa(tableID)
	}

	tbl := xlsxTable{
		XMLNS:       NameSpaceSpreadSheet.Value,
		ID:          tableID,
		Name:        name,
		DisplayName: name,
		Ref:         ref,
		AutoFilter: &xlsxAutoFilter{
			Ref: ref,
		},
		TableColumns: &xlsxTableColumns{
			Count:       len(tableColumn),
			TableColumn: tableColumn,
		},
		TableStyleInfo: &xlsxTableStyleInfo{
			Name:              options.StyleName,
			ShowFirstColumn:   options.ShowFirstColumn,
			ShowLastColumn:    options.ShowLastColumn,
			ShowRowStripes:    *options.ShowRowStripes,
			ShowColumnStripes: options.ShowColumnStripes,
		},
	}

	sheetRelationshipsTableXML := "../tables/table" + strconv.Itoa(tableID) + ".xml"
	tableXML := strings.ReplaceAll(sheetRelationshipsTableXML, "..", "xl")

	// Add first table for given sheet
	sheetPath := sw.file.sheetMap[sw.Sheet]
	sheetRels := "xl/worksheets/_rels/" + strings.TrimPrefix(sheetPath, "xl/worksheets/") + ".rels"
	rID := sw.file.addRels(sheetRels, SourceRelationshipTable, sheetRelationshipsTableXML, "")

	sw.tableParts = fmt.Sprintf(`<tableParts count="1"><tablePart r:id="rId%d"></tablePart></tableParts>`, rID)

	if err = sw.file.addContentTypePart(tableID, "table"); err != nil {
		return err
	}
	b, _ := xml.Marshal(tbl)
	sw.file.saveFileList(tableXML, b)
	return err
}

// Extract values from a row in the StreamWriter.
func (sw *StreamWriter) getRowValues(hRow, hCol, vCol int) (res []string, err error) {
	res = make([]string, vCol-hCol+1)

	r, err := sw.rawData.Reader()
	if err != nil {
		return nil, err
	}

	dec := sw.file.xmlNewDecoder(r)
	for {
		token, err := dec.Token()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return nil, err
		}
		startElement, ok := getRowElement(token, hRow)
		if !ok {
			continue
		}
		// decode cells
		var row xlsxRow
		if err := dec.DecodeElement(&row, &startElement); err != nil {
			return nil, err
		}
		for _, c := range row.C {
			col, _, err := CellNameToCoordinates(c.R)
			if err != nil {
				return nil, err
			}
			if col < hCol || col > vCol {
				continue
			}
			res[col-hCol], _ = c.getValueFrom(sw.file, nil, false)
		}
		return res, nil
	}
}

// Check if the token is an worksheet row with the matching row number.
func getRowElement(token xml.Token, hRow int) (startElement xml.StartElement, ok bool) {
	startElement, ok = token.(xml.StartElement)
	if !ok {
		return
	}
	ok = startElement.Name.Local == "row"
	if !ok {
		return
	}
	ok = false
	for _, attr := range startElement.Attr {
		if attr.Name.Local != "r" {
			continue
		}
		row, _ := strconv.Atoi(attr.Value)
		if row == hRow {
			ok = true
			return
		}
	}
	return
}

// Cell can be used directly in StreamWriter.SetRow to specify a style and
// a value.
type Cell struct {
	StyleID int
	Formula string
	Value   interface{}
}

// RowOpts define the options for the set row, it can be used directly in
// StreamWriter.SetRow to specify the style and properties of the row.
type RowOpts struct {
	Height       float64
	Hidden       bool
	StyleID      int
	OutlineLevel int
}

// marshalAttrs prepare attributes of the row.
func (r *RowOpts) marshalAttrs() (strings.Builder, error) {
	var (
		err   error
		attrs strings.Builder
	)
	if r == nil {
		return attrs, err
	}
	if r.Height > MaxRowHeight {
		err = ErrMaxRowHeight
		return attrs, err
	}
	if r.OutlineLevel > 7 {
		err = ErrOutlineLevel
		return attrs, err
	}
	if r.StyleID > 0 {
		attrs.WriteString(` s="`)
		attrs.WriteString(strconv.Itoa(r.StyleID))
		attrs.WriteString(`" customFormat="1"`)
	}
	if r.Height > 0 {
		attrs.WriteString(` ht="`)
		attrs.WriteString(strconv.FormatFloat(r.Height, 'f', -1, 64))
		attrs.WriteString(`" customHeight="1"`)
	}
	if r.OutlineLevel > 0 {
		attrs.WriteString(` outlineLevel="`)
		attrs.WriteString(strconv.Itoa(r.OutlineLevel))
		attrs.WriteString(`"`)
	}
	if r.Hidden {
		attrs.WriteString(` hidden="1"`)
	}
	return attrs, err
}

// parseRowOpts provides a function to parse the optional settings for
// *StreamWriter.SetRow.
func parseRowOpts(opts ...RowOpts) *RowOpts {
	options := &RowOpts{}
	for _, opt := range opts {
		options = &opt
	}
	return options
}

// SetRow writes an array to stream rows by giving starting cell reference and a
// pointer to an array of values. Note that you must call the 'Flush' function
// to end the streaming writing process.
//
// As a special case, if Cell is used as a value, then the Cell.StyleID will be
// applied to that cell.
func (sw *StreamWriter) SetRow(cell string, values []interface{}, opts ...RowOpts) error {
	col, row, err := CellNameToCoordinates(cell)
	if err != nil {
		return err
	}
	if row <= sw.rows {
		return newStreamSetRowError(row)
	}
	sw.rows = row
	sw.writeSheetData()
	options := parseRowOpts(opts...)
	attrs, err := options.marshalAttrs()
	if err != nil {
		return err
	}
	_, _ = sw.rawData.WriteString(`<row r="`)
	_, _ = sw.rawData.WriteString(strconv.Itoa(row))
	_, _ = sw.rawData.WriteString(`"`)
	_, _ = sw.rawData.WriteString(attrs.String())
	_, _ = sw.rawData.WriteString(`>`)
	for i, val := range values {
		if val == nil {
			continue
		}
		ref, err := CoordinatesToCellName(col+i, row)
		if err != nil {
			return err
		}
		c := xlsxC{R: ref, S: options.StyleID}
		if v, ok := val.(Cell); ok {
			c.S = v.StyleID
			val = v.Value
			setCellFormula(&c, v.Formula)
		} else if v, ok := val.(*Cell); ok && v != nil {
			c.S = v.StyleID
			val = v.Value
			setCellFormula(&c, v.Formula)
		}
		if err = sw.setCellValFunc(&c, val); err != nil {
			_, _ = sw.rawData.WriteString(`</row>`)
			return err
		}
		writeCell(&sw.rawData, c)
	}
	_, _ = sw.rawData.WriteString(`</row>`)
	return sw.rawData.Sync()
}

// SetColWidth provides a function to set the width of a single column or
// multiple columns for the StreamWriter. Note that you must call
// the 'SetColWidth' function before the 'SetRow' function. For example set
// the width column B:C as 20:
//
//	err := sw.SetColWidth(2, 3, 20)
func (sw *StreamWriter) SetColWidth(minVal, maxVal int, width float64) error {
	if sw.sheetWritten {
		return ErrStreamSetColWidth
	}
	if minVal < MinColumns || minVal > MaxColumns || maxVal < MinColumns || maxVal > MaxColumns {
		return ErrColumnNumber
	}
	if width > MaxColumnWidth {
		return ErrColumnWidth
	}
	if minVal > maxVal {
		minVal, maxVal = maxVal, minVal
	}

	sw.cols.WriteString(`<col min="`)
	sw.cols.WriteString(strconv.Itoa(minVal))
	sw.cols.WriteString(`" max="`)
	sw.cols.WriteString(strconv.Itoa(maxVal))
	sw.cols.WriteString(`" width="`)
	sw.cols.WriteString(strconv.FormatFloat(width, 'f', -1, 64))
	sw.cols.WriteString(`" customWidth="1"/>`)
	return nil
}

// InsertPageBreak creates a page break to determine where the printed page ends
// and where begins the next one by a given cell reference, the content before
// the page break will be printed on one page and after the page break on
// another.
func (sw *StreamWriter) InsertPageBreak(cell string) error {
	return sw.worksheet.insertPageBreak(cell)
}

// SetPanes provides a function to create and remove freeze panes and split
// panes by giving panes options for the StreamWriter. Note that you must call
// the 'SetPanes' function before the 'SetRow' function.
func (sw *StreamWriter) SetPanes(panes *Panes) error {
	if sw.sheetWritten {
		return ErrStreamSetPanes
	}
	return sw.worksheet.setPanes(panes)
}

// MergeCell provides a function to merge cells by a given range reference for
// the StreamWriter. Don't create a merged cell that overlaps with another
// existing merged cell.
func (sw *StreamWriter) MergeCell(topLeftCell, bottomRightCell string) error {
	_, err := cellRefsToCoordinates(topLeftCell, bottomRightCell)
	if err != nil {
		return err
	}
	sw.mergeCellsCount++
	_, _ = sw.mergeCells.WriteString(`<mergeCell ref="`)
	_, _ = sw.mergeCells.WriteString(topLeftCell)
	_, _ = sw.mergeCells.WriteString(`:`)
	_, _ = sw.mergeCells.WriteString(bottomRightCell)
	_, _ = sw.mergeCells.WriteString(`"/>`)
	return nil
}

// setCellFormula provides a function to set formula of a cell.
func setCellFormula(c *xlsxC, formula string) {
	if formula != "" {
		c.T, c.F = "str", &xlsxF{Content: formula}
	}
}

// setCellTime provides a function to set number of a cell with a time.
func (sw *StreamWriter) setCellTime(c *xlsxC, val time.Time) error {
	var date1904, isNum bool
	wb, err := sw.file.workbookReader()
	if err != nil {
		return err
	}
	if wb != nil && wb.WorkbookPr != nil {
		date1904 = wb.WorkbookPr.Date1904
	}
	if isNum, err = c.setCellTime(val, date1904); err == nil && isNum && c.S == 0 {
		style, _ := sw.file.NewStyle(&Style{NumFmt: 22})
		c.S = style
	}
	return nil
}

// setCellValFunc provides a function to set value of a cell.
func (sw *StreamWriter) setCellValFunc(c *xlsxC, val interface{}) error {
	var err error
	switch val := val.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		setCellIntFunc(c, val)
	case float32:
		c.setCellFloat(float64(val), -1, 32)
	case float64:
		c.setCellFloat(val, -1, 64)
	case string:
		c.setCellValue(val)
	case []byte:
		c.setCellValue(string(val))
	case time.Duration:
		c.T, c.V = setCellDuration(val)
	case time.Time:
		err = sw.setCellTime(c, val)
	case bool:
		c.T, c.V = setCellBool(val)
	case nil:
		return err
	case []RichTextRun:
		c.T, c.IS = "inlineStr", &xlsxSI{}
		c.IS.R, err = setRichText(val)
	default:
		c.setCellValue(fmt.Sprint(val))
	}
	return err
}

// setCellIntFunc is a wrapper of SetCellInt.
func setCellIntFunc(c *xlsxC, val interface{}) {
	switch val := val.(type) {
	case int:
		c.T, c.V = setCellInt(int64(val))
	case int8:
		c.T, c.V = setCellInt(int64(val))
	case int16:
		c.T, c.V = setCellInt(int64(val))
	case int32:
		c.T, c.V = setCellInt(int64(val))
	case int64:
		c.T, c.V = setCellInt(val)
	case uint:
		c.T, c.V = setCellUint(uint64(val))
	case uint8:
		c.T, c.V = setCellUint(uint64(val))
	case uint16:
		c.T, c.V = setCellUint(uint64(val))
	case uint32:
		c.T, c.V = setCellUint(uint64(val))
	case uint64:
		c.T, c.V = setCellUint(val)
	}
}

// writeCell constructs a cell XML and writes it to the buffer.
func writeCell(buf *bufferedWriter, c xlsxC) {
	_, _ = buf.WriteString(`<c`)
	if c.XMLSpace.Value != "" {
		_, _ = buf.WriteString(` xml:`)
		_, _ = buf.WriteString(c.XMLSpace.Name.Local)
		_, _ = buf.WriteString(`="`)
		_, _ = buf.WriteString(c.XMLSpace.Value)
		_, _ = buf.WriteString(`"`)
	}
	_, _ = buf.WriteString(` r="`)
	_, _ = buf.WriteString(c.R)
	_, _ = buf.WriteString(`"`)
	if c.S != 0 {
		_, _ = buf.WriteString(` s="`)
		_, _ = buf.WriteString(strconv.Itoa(c.S))
		_, _ = buf.WriteString(`"`)
	}
	if c.T != "" {
		_, _ = buf.WriteString(` t="`)
		_, _ = buf.WriteString(c.T)
		_, _ = buf.WriteString(`"`)
	}
	_, _ = buf.WriteString(`>`)
	if c.F != nil {
		_, _ = buf.WriteString(`<f>`)
		_ = xml.EscapeText(buf, []byte(c.F.Content))
		_, _ = buf.WriteString(`</f>`)
	}
	if c.V != "" {
		_, _ = buf.WriteString(`<v>`)
		_ = xml.EscapeText(buf, []byte(c.V))
		_, _ = buf.WriteString(`</v>`)
	}
	if c.IS != nil {
		if len(c.IS.R) > 0 {
			is, _ := xml.Marshal(c.IS.R)
			_, _ = buf.WriteString(`<is>`)
			_, _ = buf.Write(is)
			_, _ = buf.WriteString(`</is>`)
		}
		if c.IS.T != nil {
			_, _ = buf.WriteString(`<is><t`)
			if c.IS.T.Space.Value != "" {
				_, _ = buf.WriteString(` xml:`)
				_, _ = buf.WriteString(c.IS.T.Space.Name.Local)
				_, _ = buf.WriteString(`="`)
				_, _ = buf.WriteString(c.IS.T.Space.Value)
				_, _ = buf.WriteString(`"`)
			}
			_, _ = buf.WriteString(`>`)
			_, _ = buf.Write([]byte(c.IS.T.Val))
			_, _ = buf.WriteString(`</t></is>`)
		}
	}
	_, _ = buf.WriteString(`</c>`)
}

// writeSheetData prepares the element preceding sheetData and writes the
// sheetData XML start element to the buffer.
func (sw *StreamWriter) writeSheetData() {
	if !sw.sheetWritten {
		bulkAppendFields(&sw.rawData, sw.worksheet, 4, 5)
		if sw.cols.Len() > 0 {
			_, _ = sw.rawData.WriteString("<cols>")
			_, _ = sw.rawData.WriteString(sw.cols.String())
			_, _ = sw.rawData.WriteString("</cols>")
		}
		_, _ = sw.rawData.WriteString(`<sheetData>`)
		sw.sheetWritten = true
	}
}

// Flush ending the streaming writing process.
func (sw *StreamWriter) Flush() error {
	sw.writeSheetData()
	_, _ = sw.rawData.WriteString(`</sheetData>`)
	bulkAppendFields(&sw.rawData, sw.worksheet, 8, 15)
	mergeCells := strings.Builder{}
	if sw.mergeCellsCount > 0 {
		_, _ = mergeCells.WriteString(`<mergeCells count="`)
		_, _ = mergeCells.WriteString(strconv.Itoa(sw.mergeCellsCount))
		_, _ = mergeCells.WriteString(`">`)
		_, _ = mergeCells.WriteString(sw.mergeCells.String())
		_, _ = mergeCells.WriteString(`</mergeCells>`)
	}
	_, _ = sw.rawData.WriteString(mergeCells.String())
	bulkAppendFields(&sw.rawData, sw.worksheet, 17, 38)
	_, _ = sw.rawData.WriteString(sw.tableParts)
	bulkAppendFields(&sw.rawData, sw.worksheet, 40, 40)
	_, _ = sw.rawData.WriteString(`</worksheet>`)
	if err := sw.rawData.Flush(); err != nil {
		return err
	}

	sheetPath := sw.file.sheetMap[sw.Sheet]
	sw.file.Sheet.Delete(sheetPath)
	sw.file.checked.Delete(sheetPath)
	sw.file.Pkg.Delete(sheetPath)

	return nil
}

// bulkAppendFields bulk-appends fields in a worksheet by specified field
// names order range.
func bulkAppendFields(w io.Writer, ws *xlsxWorksheet, from, to int) {
	s := reflect.ValueOf(ws).Elem()
	enc := xml.NewEncoder(w)
	for i := 0; i < s.NumField(); i++ {
		if from <= i && i <= to {
			_ = enc.Encode(s.Field(i).Interface())
		}
	}
}

// bufferedWriter uses a temp file to store an extended buffer. Writes are
// always made to an in-memory buffer, which will always succeed. The buffer
// is written to the temp file with Sync, which may return an error.
// Therefore, Sync should be periodically called and the error checked.
type bufferedWriter struct {
	tmp *os.File
	buf bytes.Buffer
}

// Write to the in-memory buffer. The error is always nil.
func (bw *bufferedWriter) Write(p []byte) (n int, err error) {
	return bw.buf.Write(p)
}

// WriteString write to the in-memory buffer. The error is always nil.
func (bw *bufferedWriter) WriteString(p string) (n int, err error) {
	return bw.buf.WriteString(p)
}

// Reader provides read-access to the underlying buffer/file.
func (bw *bufferedWriter) Reader() (io.Reader, error) {
	if bw.tmp == nil {
		return bytes.NewReader(bw.buf.Bytes()), nil
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	fi, err := bw.tmp.Stat()
	if err != nil {
		return nil, err
	}
	// os.File.ReadAt does not affect the cursor position and is safe to use here
	return io.NewSectionReader(bw.tmp, 0, fi.Size()), nil
}

// Sync will write the in-memory buffer to a temp file, if the in-memory
// buffer has grown large enough. Any error will be returned.
func (bw *bufferedWriter) Sync() (err error) {
	// Try to use local storage
	if bw.buf.Len() < StreamChunkSize {
		return nil
	}
	if bw.tmp == nil {
		bw.tmp, err = os.CreateTemp(os.TempDir(), "excelize-")
		if err != nil {
			// can not use local storage
			return nil
		}
	}
	return bw.Flush()
}

// Flush the entire in-memory buffer to the temp file, if a temp file is being
// used.
func (bw *bufferedWriter) Flush() error {
	if bw.tmp == nil {
		return nil
	}
	_, err := bw.buf.WriteTo(bw.tmp)
	if err != nil {
		return err
	}
	bw.buf.Reset()
	return nil
}

// Close the underlying temp file and reset the in-memory buffer.
func (bw *bufferedWriter) Close() error {
	bw.buf.Reset()
	if bw.tmp == nil {
		return nil
	}
	defer os.Remove(bw.tmp.Name())
	return bw.tmp.Close()
}
