package glog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	pool *sync.Pool
)

type Severity int32

func init() {
	pool = &sync.Pool{
		New: func() interface{} {
			return new(buffer)
		},
	}
}

const (
	DebugLog Severity = iota
	InfoLog
	WarningLog
	ErrorLog
	FatalLog
)

const severityChar = "DIWEF"

type flushSyncWriter interface {
	Flush() error
	Sync() error
	io.Writer
}

func (l *loggingT) Flush() {
	l.lockAndFlushAll()
}

// lockAndFlushAll is like flushAll but locks l.mu first.
func (l *loggingT) lockAndFlushAll() {
	l.mu.Lock()
	l.flushAll()
	l.mu.Unlock()
}

// flushAll flushes all the logs and attempts to "sync" their data to disk.
// l.mu is held.
func (l *loggingT) flushAll() {
	// Flush from fatal down, in case there's trouble flushing.
	if l.file != nil {
		l.file.Flush() // ignore error
		l.file.Sync()  // ignore error
	}
}

func (l *loggingT) flushDaemon(flushInterval int) {
	for range time.NewTicker(time.Duration(flushInterval) * time.Second).C {
		l.lockAndFlushAll()
	}
}

type loggingT struct {
	logPath     string
	logLevel    Severity
	fileMaxSize uint64 //flushInterval int
	mu          sync.Mutex
	file        flushSyncWriter // syncBuffer
}

func NewLogger(logPath, fileMaxSize string, logLevel Severity, flushInterval int) *loggingT {
	n := unitConv(fileMaxSize)
	logger := &loggingT{
		logPath:     logPath,
		logLevel:    logLevel,
		fileMaxSize: n,
	}
	go logger.flushDaemon(flushInterval)
	return logger
}

func stringToInt(s string) (n uint64, err error) {
	for _, ch := range s {
		ch -= '0'
		if ch > 9 {
			return n, errors.New("")
		}
		n = n*10 + uint64(ch)
	}
	return n, nil
}

func unitConv(s string) uint64 {
	n, err := stringToInt(s[:len(s)-1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "The logfile size %s is incorrect", s)
		os.Exit(1)
	}
	switch s[len(s)-1] {
	case 'G', 'g':
		return n * 1024 * 1024 * 1024
	case 'M', 'm':
		return n * 1024 * 1024
	case 'K', 'k':
		return n * 1024
	default:
		fmt.Fprintf(os.Stderr, "The logfile size %s is incorrect", s)
		os.Exit(1)
	}
	return 0
}

func (l *loggingT) SetLevel(level Severity) {
	l.logLevel = level
}

func (l *loggingT) header(s Severity, depth int) (*buffer, string, int) {
	_, file, line, ok := runtime.Caller(3 + depth)
	if !ok {
		file = "???"
		line = 1
	} else {
		slash := strings.LastIndex(file, "/")
		if slash >= 0 {
			file = file[slash+1:]
		}
	}
	return l.formatHeader(s, file, line), file, line
}
func (l *loggingT) exit(err error) {
	fmt.Fprintf(os.Stderr, "log: exiting because of error: %s\n", err)
	l.flushAll()
	os.Exit(2)
}

func (l *loggingT) output(buf *buffer, file string, line int) {
	data := buf.Bytes()
	l.mu.Lock()
	if l.file == nil {
		sb := &syncBuffer{
			logger: l,
		}
		if err := sb.create(); err != nil {
			os.Stderr.Write(data) // Make sure the message appears somewhere.
			l.exit(err)
		}
	}
	l.file.Write(data)
	l.mu.Unlock()
	l.putBuffer(buf)
}

type syncBuffer struct {
	logger *loggingT
	*bufio.Writer
	file   *os.File
	num    int32
	nbytes uint64 // The number of bytes written to this file
}

func (sb *syncBuffer) rotateFile(now time.Time) error {
	sb.Flush()
	sb.file.Close()
	filePath := fmt.Sprintf("%s.%d%d%d.%d%d%d", sb.logger.logPath, now.Year(),
		now.Month(), now.Day(), now.Hour(), now.Minute(), now.Day())
	if err := os.Rename(sb.logger.logPath, filePath); err != nil {
		return err
	}
	return sb.create()

}

func (sb *syncBuffer) create() (err error) {
	sb.file, err = os.Create(sb.logger.logPath)
	if err != nil {
		return err
	}
	//atomic.StoreUint64(&sb.nbytes, 0)
	sb.nbytes = 0
	sb.logger.file = sb
	sb.Writer = bufio.NewWriterSize(sb.file, bufferSize)
	return nil
}

func (sb *syncBuffer) Sync() error {
	return sb.file.Sync()
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	if sb.nbytes+uint64(len(p)) >= sb.logger.fileMaxSize {
		if err := sb.rotateFile(time.Now()); err != nil {
			sb.logger.exit(err)
		}
	}
	n, err = sb.Writer.Write(p)
	sb.nbytes += uint64(n)
	if err != nil {
		sb.logger.exit(err)
	}
	return
}

const bufferSize = 256 * 1024

var timeNow = time.Now

type buffer struct {
	bytes.Buffer
	tmp [29]byte // temporary byte array for creating headers.
}

func (l *loggingT) getBuffer() *buffer {
	return pool.Get().(*buffer)
}

func (l *loggingT) putBuffer(buf *buffer) {
	buf.Buffer.Reset()
	pool.Put(buf)
}

func (l *loggingT) formatHeader(s Severity, file string, line int) *buffer {
	now := timeNow()
	if line < 0 {
		line = 0 // not a real line number, but acceptable to someDigits
	}
	if s > FatalLog {
		s = FatalLog // for safety.
	}
	buf := l.getBuffer()

	// Avoid Fprintf, for speed. The format is so simple that we can do it quickly by hand.
	// It's worth about 3X. Fprintf is hard.
	year, month, day := now.Date()
	hour, minute, second := now.Clock()
	// Lmmdd hh:mm:ss.uuuuuu threadid file:line]
	buf.tmp[0] = severityChar[s]
	buf.nDigits(4, 1, year, '0')
	buf.tmp[5] = '-'
	buf.twoDigits(6, int(month))
	buf.tmp[8] = '-'
	buf.twoDigits(9, day)
	buf.tmp[11] = ' '
	buf.twoDigits(12, hour)
	buf.tmp[14] = ':'
	buf.twoDigits(15, minute)
	buf.tmp[17] = ':'
	buf.twoDigits(18, second)
	buf.tmp[20] = '.'
	buf.nDigits(6, 21, now.Nanosecond()/1000, '0')
	buf.tmp[27] = ' '
	buf.Write(buf.tmp[:28])
	buf.WriteString(file)
	buf.tmp[0] = ':'
	n := buf.someDigits(1, line)
	buf.tmp[n+1] = ']'
	buf.tmp[n+2] = ' '
	buf.Write(buf.tmp[:n+3])
	return buf
}

const digits = "0123456789"

// twoDigits formats a zero-prefixed two-digit integer at buf.tmp[i].
func (buf *buffer) twoDigits(i, d int) {
	buf.tmp[i+1] = digits[d%10]
	d /= 10
	buf.tmp[i] = digits[d%10]
}

// nDigits formats an n-digit integer at buf.tmp[i],
// padding with pad on the left.
// It assumes d >= 0.
func (buf *buffer) nDigits(n, i, d int, pad byte) {
	j := n - 1
	for ; j >= 0 && d > 0; j-- {
		buf.tmp[i+j] = digits[d%10]
		d /= 10
	}
	for ; j >= 0; j-- {
		buf.tmp[i+j] = pad
	}
}

// someDigits formats a zero-prefixed variable-width integer at buf.tmp[i].
func (buf *buffer) someDigits(i, d int) int {
	// Print into the top, then copy down. We know there's space for at least
	// a 10-digit number.
	j := len(buf.tmp)
	for {
		j--
		buf.tmp[j] = digits[d%10]
		d /= 10
		if d == 0 {
			break
		}
	}
	return copy(buf.tmp[i:], buf.tmp[j:])
}

func (l *loggingT) println(s Severity, args ...interface{}) {
	buf, file, line := l.header(s, 0)
	fmt.Fprintln(buf, args...)
	l.output(buf, file, line)
}

func (l *loggingT) print(s Severity, args ...interface{}) {
	l.printDepth(s, 1, args...)
}

func (l *loggingT) printDepth(s Severity, depth int, args ...interface{}) {
	buf, file, line := l.header(s, depth)
	fmt.Fprint(buf, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(buf, file, line)
}

func (l *loggingT) printfDepth(s Severity, depth int, format string, args ...interface{}) {
	buf, file, line := l.header(s, depth)
	fmt.Fprintf(buf, format, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(buf, file, line)
}

func (l *loggingT) printf(s Severity, format string, args ...interface{}) {
	buf, file, line := l.header(s, 0)
	fmt.Fprintf(buf, format, args...)
	if buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	l.output(buf, file, line)
}

func (l *loggingT) Debugf(format string, args ...interface{}) {
	if DebugLog >= l.logLevel {
		l.printf(DebugLog, format, args...)
	}
}

func (l *loggingT) Infof(format string, args ...interface{}) {
	if InfoLog >= l.logLevel {
		l.printf(InfoLog, format, args...)
	}
}

func (l *loggingT) Warningf(format string, args ...interface{}) {
	if WarningLog >= l.logLevel {
		l.printf(WarningLog, format, args...)
	}
}

func (l *loggingT) Errorf(format string, args ...interface{}) {
	if ErrorLog >= l.logLevel {
		l.printf(ErrorLog, format, args...)
	}
}

func (l *loggingT) Fatalf(format string, args ...interface{}) {
	if FatalLog >= l.logLevel {
		l.printf(FatalLog, format, args...)
		l.exit(errors.New(""))
	}
}

func (l *loggingT) Debug(args ...interface{}) {
	if DebugLog >= l.logLevel {
		l.print(DebugLog, args...)
	}
}

func (l *loggingT) Info(args ...interface{}) {
	if InfoLog >= l.logLevel {
		l.print(InfoLog, args...)
	}
}

func (l *loggingT) Warning(args ...interface{}) {
	if WarningLog >= l.logLevel {
		l.print(WarningLog, args...)
	}
}

func (l *loggingT) Error(args ...interface{}) {
	if ErrorLog >= l.logLevel {
		l.print(ErrorLog, args...)
	}
}

func (l *loggingT) Fatal(args ...interface{}) {
	if FatalLog >= l.logLevel {
		l.print(FatalLog, args...)
		l.exit(errors.New(""))
	}
}

func (l *loggingT) DebugDepth(depth int, args ...interface{}) {
	if DebugLog >= l.logLevel {
		l.printDepth(DebugLog, depth, args...)
	}
}

func (l *loggingT) InfoDepth(depth int, args ...interface{}) {
	if InfoLog >= l.logLevel {
		l.printDepth(InfoLog, depth, args...)
	}
}

func (l *loggingT) WarningDepth(depth int, args ...interface{}) {
	if WarningLog >= l.logLevel {
		l.printDepth(WarningLog, depth, args...)
	}
}

func (l *loggingT) ErrorDepth(depth int, args ...interface{}) {
	if ErrorLog >= l.logLevel {
		l.printDepth(ErrorLog, depth, args...)
	}
}

func (l *loggingT) FatalDepth(depth int, args ...interface{}) {
	if FatalLog >= l.logLevel {
		l.printDepth(FatalLog, depth, args...)
		l.exit(errors.New(""))
	}
}

func (l *loggingT) DebugfDepth(depth int, format string, args ...interface{}) {
	if DebugLog >= l.logLevel {
		l.printfDepth(DebugLog, depth, format, args...)
	}
}

func (l *loggingT) InfofDepth(depth int, format string, args ...interface{}) {
	if InfoLog >= l.logLevel {
		l.printfDepth(InfoLog, depth, format, args...)
	}
}

func (l *loggingT) WarningfDepth(depth int, format string, args ...interface{}) {
	if WarningLog >= l.logLevel {
		l.printfDepth(WarningLog, depth, format, args...)
	}
}

func (l *loggingT) ErrorfDepth(depth int, format string, args ...interface{}) {
	if ErrorLog >= l.logLevel {
		l.printfDepth(ErrorLog, depth, format, args...)
	}
}

func (l *loggingT) FatalfDepth(depth int, format string, args ...interface{}) {
	if FatalLog >= l.logLevel {
		l.printfDepth(FatalLog, depth, format, args...)
		l.exit(errors.New(""))
	}
}
