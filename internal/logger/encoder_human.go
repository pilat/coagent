package logger

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// Column widths matching the old StructuredLogger.
const (
	levelWidth     = 5
	componentWidth = 12
	messageWidth   = 25
)

var _pool = buffer.NewPool()

// Colors (ANSI 256 palette).
var (
	timeStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	levelInfoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	levelWarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	levelDbgStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	levelErrStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	compStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	msgStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	detailStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// humanEncoder renders log entries as colorful fixed-width columns.
type humanEncoder struct {
	// fields accumulated via With() — rendered as key=value pairs.
	fields []field
}

type field struct {
	key string
	val string
}

// sliceEncoder collects array elements as strings.
type sliceEncoder struct {
	elems []string
}

var _ zapcore.Encoder = (*humanEncoder)(nil)

func newHumanEncoder() zapcore.Encoder {
	return &humanEncoder{}
}

func (e *humanEncoder) Clone() zapcore.Encoder {
	clone := &humanEncoder{
		fields: make([]field, len(e.fields)),
	}
	copy(clone.fields, e.fields)

	return clone
}

func (e *humanEncoder) EncodeEntry(entry zapcore.Entry, extra []zapcore.Field) (*buffer.Buffer, error) {
	buf := _pool.Get()

	buf.AppendString(timeStyle.Render(entry.Time.Format("15:04:05")))
	buf.AppendByte(' ')

	buf.AppendString(renderLevel(entry.Level))
	buf.AppendByte(' ')

	if entry.LoggerName != "" {
		buf.AppendString(compStyle.Render(padRight(entry.LoggerName, componentWidth)))
		buf.AppendByte(' ')
	}

	buf.AppendString(msgStyle.Render(padRight(entry.Message, messageWidth)))

	enc, ok := e.Clone().(*humanEncoder)
	if !ok {
		enc = &humanEncoder{}
	}

	for _, f := range extra {
		enc.addField(f.Key, fieldValue(f))
	}

	if len(enc.fields) > 0 {
		buf.AppendByte(' ')
		buf.AppendString(enc.renderFields())
	}

	buf.AppendByte('\n')

	return buf, nil
}

// fieldValue extracts a string representation from a zapcore.Field.
func fieldValue(f zapcore.Field) string {
	switch f.Type {
	case zapcore.StringType:
		return f.String
	case zapcore.Int64Type, zapcore.Int32Type, zapcore.Int16Type, zapcore.Int8Type:
		return strconv.FormatInt(f.Integer, 10)
	case zapcore.Uint64Type, zapcore.Uint32Type, zapcore.Uint16Type, zapcore.Uint8Type:
		return strconv.FormatInt(f.Integer, 10)
	case zapcore.Float64Type:
		return fmt.Sprintf("%g", math.Float64frombits(uint64(f.Integer)))
	case zapcore.Float32Type:
		return fmt.Sprintf("%g", math.Float32frombits(uint32(f.Integer)))
	case zapcore.BoolType:
		if f.Integer == 1 {
			return "true"
		}

		return "false"
	case zapcore.ErrorType:
		if f.Interface == nil {
			return "<nil>"
		}

		if err, ok := f.Interface.(error); ok {
			return err.Error()
		}

		return fmt.Sprintf("%v", f.Interface)
	case zapcore.DurationType:
		return time.Duration(f.Integer).String()
	case zapcore.UnknownType, zapcore.ArrayMarshalerType, zapcore.ObjectMarshalerType, zapcore.BinaryType,
		zapcore.ByteStringType, zapcore.Complex128Type, zapcore.Complex64Type, zapcore.TimeType, zapcore.TimeFullType,
		zapcore.UintptrType, zapcore.ReflectType, zapcore.NamespaceType, zapcore.StringerType, zapcore.SkipType,
		zapcore.InlineMarshalerType:
	}

	if f.Interface != nil {
		return fmt.Sprintf("%v", f.Interface)
	}

	return f.String
}

// Implement zapcore.Encoder interface methods that accumulate fields.
// These are called when zap's With() adds fields to the encoder.

func (e *humanEncoder) AddString(key, val string)          { e.addField(key, val) }
func (e *humanEncoder) AddBool(key string, val bool)       { e.addField(key, strconv.FormatBool(val)) }
func (e *humanEncoder) AddInt64(key string, val int64)     { e.addField(key, strconv.FormatInt(val, 10)) }
func (e *humanEncoder) AddFloat64(key string, val float64) { e.addField(key, fmt.Sprintf("%g", val)) }
func (e *humanEncoder) AddUint64(key string, val uint64) {
	e.addField(key, strconv.FormatUint(val, 10))
}

func (e *humanEncoder) AddDuration(key string, val time.Duration) {
	e.addField(key, val.String())
}

func (e *humanEncoder) AddTime(key string, val time.Time) {
	e.addField(key, val.Format(time.RFC3339))
}

func renderLevel(lvl zapcore.Level) string {
	var s lipgloss.Style

	switch lvl {
	case zapcore.InfoLevel:
		s = levelInfoStyle
	case zapcore.WarnLevel:
		s = levelWarnStyle
	case zapcore.DebugLevel:
		s = levelDbgStyle
	case zapcore.ErrorLevel:
		s = levelErrStyle
	case zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel, zapcore.InvalidLevel:
		s = levelDbgStyle
	default:
		s = levelDbgStyle
	}

	return s.Render(padRight(strings.ToUpper(lvl.String()), levelWidth))
}

// The remaining PrimitiveArrayEncoder and ObjectEncoder methods — we provide
// no-ops or simple implementations for completeness.

func (e *humanEncoder) AddArray(key string, arr zapcore.ArrayMarshaler) error {
	enc := &sliceEncoder{}
	if err := arr.MarshalLogArray(enc); err != nil {
		return err
	}

	e.addField(key, fmt.Sprintf("[%s]", strings.Join(enc.elems, ",")))

	return nil
}

func (e *humanEncoder) AddObject(key string, obj zapcore.ObjectMarshaler) error {
	m := zapcore.NewMapObjectEncoder()
	if err := obj.MarshalLogObject(m); err != nil {
		return err
	}

	e.addField(key, fmt.Sprintf("%v", m.Fields))

	return nil
}

func (e *humanEncoder) AddByteString(key string, val []byte) { e.addField(key, string(val)) }
func (e *humanEncoder) AddComplex128(key string, val complex128) {
	e.addField(key, fmt.Sprintf("%v", val))
}

func (e *humanEncoder) AddComplex64(key string, val complex64) {
	e.addField(key, fmt.Sprintf("%v", val))
}
func (e *humanEncoder) AddFloat32(key string, val float32) { e.addField(key, fmt.Sprintf("%g", val)) }
func (e *humanEncoder) AddInt(key string, val int)         { e.addField(key, strconv.Itoa(val)) }
func (e *humanEncoder) AddInt32(key string, val int32)     { e.addField(key, strconv.Itoa(int(val))) }
func (e *humanEncoder) AddInt16(key string, val int16)     { e.addField(key, strconv.Itoa(int(val))) }
func (e *humanEncoder) AddInt8(key string, val int8)       { e.addField(key, strconv.Itoa(int(val))) }
func (e *humanEncoder) AddUint(key string, val uint) {
	e.addField(key, strconv.FormatUint(uint64(val), 10))
}

func (e *humanEncoder) AddUint32(key string, val uint32) {
	e.addField(key, strconv.FormatUint(uint64(val), 10))
}

func (e *humanEncoder) AddUint16(key string, val uint16) {
	e.addField(key, strconv.FormatUint(uint64(val), 10))
}

func (e *humanEncoder) AddUint8(key string, val uint8) {
	e.addField(key, strconv.FormatUint(uint64(val), 10))
}
func (e *humanEncoder) AddUintptr(key string, val uintptr) { e.addField(key, fmt.Sprintf("%d", val)) }
func (e *humanEncoder) AddReflected(key string, val any) error {
	e.addField(key, fmt.Sprintf("%v", val))
	return nil
}
func (e *humanEncoder) AddBinary(key string, val []byte) { e.addField(key, hex.EncodeToString(val)) }
func (e *humanEncoder) OpenNamespace(key string)         {}

func (e *humanEncoder) addField(key, val string) {
	e.fields = append(e.fields, field{key: key, val: val})
}

func (e *humanEncoder) renderFields() string {
	pairs := make([]string, 0, len(e.fields))
	for _, f := range e.fields {
		pairs = append(pairs, detailStyle.Render(fmt.Sprintf("%s=%s", f.key, f.val)))
	}

	return strings.Join(pairs, " ")
}

func (s *sliceEncoder) AppendBool(v bool)         { s.elems = append(s.elems, strconv.FormatBool(v)) }
func (s *sliceEncoder) AppendByteString(v []byte) { s.elems = append(s.elems, string(v)) }
func (s *sliceEncoder) AppendComplex128(v complex128) {
	s.elems = append(s.elems, fmt.Sprintf("%v", v))
}
func (s *sliceEncoder) AppendComplex64(v complex64) { s.elems = append(s.elems, fmt.Sprintf("%v", v)) }
func (s *sliceEncoder) AppendFloat64(v float64)     { s.elems = append(s.elems, fmt.Sprintf("%g", v)) }
func (s *sliceEncoder) AppendFloat32(v float32)     { s.elems = append(s.elems, fmt.Sprintf("%g", v)) }
func (s *sliceEncoder) AppendInt(v int)             { s.elems = append(s.elems, strconv.Itoa(v)) }
func (s *sliceEncoder) AppendInt64(v int64)         { s.elems = append(s.elems, strconv.FormatInt(v, 10)) }
func (s *sliceEncoder) AppendInt32(v int32)         { s.elems = append(s.elems, strconv.Itoa(int(v))) }
func (s *sliceEncoder) AppendInt16(v int16)         { s.elems = append(s.elems, strconv.Itoa(int(v))) }
func (s *sliceEncoder) AppendInt8(v int8)           { s.elems = append(s.elems, strconv.Itoa(int(v))) }
func (s *sliceEncoder) AppendString(v string)       { s.elems = append(s.elems, v) }
func (s *sliceEncoder) AppendUint(v uint) {
	s.elems = append(s.elems, strconv.FormatUint(uint64(v), 10))
}
func (s *sliceEncoder) AppendUint64(v uint64) { s.elems = append(s.elems, strconv.FormatUint(v, 10)) }
func (s *sliceEncoder) AppendUint32(v uint32) {
	s.elems = append(s.elems, strconv.FormatUint(uint64(v), 10))
}

func (s *sliceEncoder) AppendUint16(v uint16) {
	s.elems = append(s.elems, strconv.FormatUint(uint64(v), 10))
}

func (s *sliceEncoder) AppendUint8(v uint8) {
	s.elems = append(s.elems, strconv.FormatUint(uint64(v), 10))
}
func (s *sliceEncoder) AppendUintptr(v uintptr) { s.elems = append(s.elems, fmt.Sprintf("%d", v)) }
func (s *sliceEncoder) AppendDuration(v time.Duration) {
	s.elems = append(s.elems, v.String())
}

func (s *sliceEncoder) AppendTime(v time.Time) {
	s.elems = append(s.elems, v.Format(time.RFC3339))
}
func (s *sliceEncoder) AppendArray(v zapcore.ArrayMarshaler) error { return v.MarshalLogArray(s) }
func (s *sliceEncoder) AppendObject(v zapcore.ObjectMarshaler) error {
	m := zapcore.NewMapObjectEncoder()
	if err := v.MarshalLogObject(m); err != nil {
		return err
	}

	s.elems = append(s.elems, fmt.Sprintf("%v", m.Fields))

	return nil
}

func (s *sliceEncoder) AppendReflected(v any) error {
	s.elems = append(s.elems, fmt.Sprintf("%v", v))
	return nil
}

// padRight pads s with spaces to width.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}

	return s + strings.Repeat(" ", width-len(s))
}
