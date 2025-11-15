package convert

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cheggaaa/pb/v3"
	"github.com/common-nighthawk/go-figure"
	"github.com/fatih/color"
	"github.com/saintfish/chardet"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// printHeader prints the application banner.
func printHeader() {
	os.Stdout.WriteString("\033[H\033[2J") // Clear screen
	banner := figure.NewFigure("LOG FORMATER", "slant", true)
	fmt.Printf("\n==================================\n%s   %s\n==================================\n\nLOADING FILES PLEASE WAIT .....\n\n",
		color.RedString(banner.String()),
		color.GreenString("by @redscorpionlogs"))
}

// quarantine moves a file to the error folder and logs the reason.
func quarantine(src, errFolder, reason string) {
	fmt.Printf("Quarantining %s → %s (%s)\n", src, errFolder, reason)
	if err := os.MkdirAll(errFolder, 0755); err != nil {
		fmt.Printf("Error creating error folder %s: %v\n", errFolder, err)
	}
	if err := shutilMove(src, filepath.Join(errFolder, filepath.Base(src))); err != nil {
		fmt.Printf("Error moving to error folder: %v\n", err)
	}
	logError(src, reason)
}

// logError writes an error message to the error log file.
func logError(src, msg string) {
	f, err := os.OpenFile("lconv_error_log.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening error log: %v\n", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "Error processing %s: %s\n", src, msg)
}

// shutilMove performs a move operation similar to Python's shutil.move.
func shutilMove(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return os.Remove(src)
}

// appendContext writes the matched line plus next 3 lines to banks.txt.
func appendContext(trigger string, sc *bufio.Scanner) {
	doneFile := filepath.Join("files", "done", "banks.txt")
	f, err := os.OpenFile(doneFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logError(trigger, fmt.Sprintf("Failed to open done file: %v", err))
		return
	}
	defer f.Close()

	fmt.Fprintln(f, trigger)
	for i := 0; i < 3 && sc.Scan(); i++ {
		fmt.Fprintln(f, strings.TrimSpace(sc.Text()))
	}
}

// saveCreds writes all credentials to the output file and returns success status.
func saveCreds(out string, creds []string) bool {
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening output file %s: %v\n", out, err)
		return false
	}
	defer f.Close()
	for _, c := range creds {
		if _, err := fmt.Fprintln(f, c); err != nil {
			fmt.Printf("Error writing to output file %s: %v\n", out, err)
			return false
		}
	}
	return true
}

// hasAny returns true if s contains any of the provided keys.
func hasAny(s string, keys ...string) bool {
	for _, k := range keys {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// tail extracts the value after ':' or '=' from a line.
func tail(line string) string {
	parts := regexp.MustCompile(`[:=]`).Split(line, -1)
	return strings.TrimSpace(parts[len(parts)-1])
}

// cleanURL extracts and cleans the URL or Host from the line.
func cleanURL(line string) string {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) < 2 {
		return ""
	}
	rhs := parts[1]
	rhsParts := strings.SplitN(rhs, "=", 2)
	value := strings.TrimSpace(rhsParts[len(rhsParts)-1])
	return strings.TrimPrefix(value, "://")
}

// processFile implements the core file processing logic.
func processFile(inputFilePath, outputFilePath, errorFolder string) {
	var (
		credentials  []string
		foundStrings bool
	)

	searchStrings := []string{
		"zemenbank", "com.boa.apollo", "hellocash.net", "unitedib", "awashonline",
		"com.cr2.amolelight", "com.cr2.dashenamole", "myamole", "com.boa.boaMobileBanking", "admin.2xsport.com",
	}
	etURLPattern := regexp.MustCompile(`dashboard.*bet.*\.et\b`)

	rawdata, err := os.ReadFile(inputFilePath)
	if err != nil {
		fmt.Printf("File not found: %s. Skipping.\n", inputFilePath)
		return
	}

	detector := chardet.NewTextDetector()
	result, err := detector.DetectBest(rawdata)
	if err != nil || result == nil {
		quarantine(inputFilePath, errorFolder, fmt.Sprintf("Error detecting encoding: %v", err))
		return
	}

	encoding := strings.ToLower(result.Charset)
	if encoding != "utf-8" {
		enc, _ := ianaindex.IANA.Encoding(strings.ToUpper(encoding))
		if enc == nil {
			quarantine(inputFilePath, errorFolder, fmt.Sprintf("Unsupported encoding: %s", encoding))
			return
		}
		utf8Bytes, _, err := transform.Bytes(enc.NewDecoder(), rawdata)
		if err != nil {
			quarantine(inputFilePath, errorFolder, fmt.Sprintf("Encoding conversion failed: %v", err))
			return
		}
		err = os.WriteFile(inputFilePath, utf8Bytes, 0644)
		if err != nil {
			quarantine(inputFilePath, errorFolder, fmt.Sprintf("Writing UTF-8 file failed: %v", err))
			return
		}
	}

	file, err := os.Open(inputFilePath)
	if err != nil {
		fmt.Printf("File not found: %s. Skipping.\n", inputFilePath)
		return
	}
	defer file.Close()

	// Count total lines for progress bar
	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}
	if err := scanner.Err(); err != nil {
		quarantine(inputFilePath, errorFolder, fmt.Sprintf("Reading file failed: %v", err))
		return
	}
	if lineCount == 0 {
		fmt.Printf("Deleting empty file %s\n", inputFilePath)
		file.Close()
		os.Remove(inputFilePath)
		return
	}

	// Reset file pointer
	file.Seek(0, io.SeekStart)
	scanner = bufio.NewScanner(file)
	bar := pb.StartNew(lineCount)

	var username, password, url string

	for scanner.Scan() {
		bar.Increment()
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "=") {
			continue
		}

		for _, s := range searchStrings {
			if strings.Contains(line, s) {
				foundStrings = true
				appendContext(line, scanner)
				break
			}
		}

		if etURLPattern.MatchString(line) {
			foundStrings = true
			appendContext(line, scanner)
		}

		switch {
		case hasAny(line, "Username", "USER", "LOGIN", "USR"):
			username = tail(line)
		case hasAny(line, "Password", "PASS"):
			password = tail(line)
		case strings.Contains(line, "URL") || strings.Contains(line, "Host"):
			url = cleanURL(line)
		}

		if username != "" && password != "" && url != "" {
			if !strings.Contains(url, "://t.me/") {
				credentials = append(credentials, fmt.Sprintf("%s:%s:%s", url, username, password))
			}
			username, password, url = "", "", ""
		}
	}
	bar.Finish()

	if err := scanner.Err(); err != nil {
		quarantine(inputFilePath, errorFolder, fmt.Sprintf("Error reading file: %v", err))
		return
	}

	var credentialsWritten bool
	if len(credentials) == 0 {
		fileInfo, err := os.Stat(inputFilePath)
		if err == nil && fileInfo.Size() == 0 {
			fmt.Printf("Deleting empty file %s\n", inputFilePath)
			if err := os.Remove(inputFilePath); err != nil {
				fmt.Printf("Error deleting empty file %s: %v\n", inputFilePath, err)
			}
			return
		} else {
			logError(inputFilePath, "No credentials found")
		}
	} else {
		credentialsWritten = saveCreds(outputFilePath, credentials)
		if credentialsWritten {
			fmt.Printf("Credentials from %s → %s\n", inputFilePath, outputFilePath)
		} else {
			logError(inputFilePath, "Failed to write credentials to output file")
		}
	}

	// Delete or move file based on processing results
	if !foundStrings && len(credentials) == 0 {
		fileInfo, err := os.Stat(inputFilePath)
		if err == nil && fileInfo.Size() != 0 {
			// Close file before deletion
			file.Close()
			fmt.Printf("Deleting file %s (no search strings found)\n", inputFilePath)
			if err := os.Remove(inputFilePath); err != nil {
				fmt.Printf("Error deleting file %s: %v\n", inputFilePath, err)
				logError(inputFilePath, fmt.Sprintf("Failed to delete file: %v", err))
			}
		}
	} else if foundStrings && len(credentials) == 0 {
		// Move to etbanks if we found search strings but no credentials
		destFolder := filepath.Join("files", "etbanks")
		if err := os.MkdirAll(destFolder, 0755); err != nil {
			fmt.Printf("Failed to create folder %s: %v\n", destFolder, err)
			return
		}
		destPath := filepath.Join(destFolder, filepath.Base(inputFilePath))
		if err := shutilMove(inputFilePath, destPath); err != nil {
			logError(inputFilePath, fmt.Sprintf("Failed moving file: %v", err))
		} else {
			fmt.Printf("Moved %s → %s\n", inputFilePath, destPath)
		}
	} else if len(credentials) > 0 && credentialsWritten {
		// Close file before deletion
		file.Close()
		// Delete file after successfully writing credentials
		fmt.Printf("Deleting processed file %s (credentials written successfully)\n", inputFilePath)
		if err := os.Remove(inputFilePath); err != nil {
			fmt.Printf("Error deleting file %s: %v\n", inputFilePath, err)
			logError(inputFilePath, fmt.Sprintf("Failed to delete file: %v", err))
		} else {
			fmt.Printf("Successfully deleted file %s\n", inputFilePath)
		}
	} else if len(credentials) > 0 && !credentialsWritten {
		// If we had credentials but failed to write them, don't delete the file
		fmt.Printf("Keeping file %s due to failed credential writing\n", inputFilePath)
	}
}

func ConvertTextFiles() error {
	printHeader()

	// Read paths from environment
	inputPath := os.Getenv("CONVERT_INPUT_DIR")    // e.g. "files/pass"
	outputFile := os.Getenv("CONVERT_OUTPUT_FILE") // e.g. "files/all_extracted.txt"
	if inputPath == "" || outputFile == "" {
		return fmt.Errorf("CONVERT_INPUT_DIR and CONVERT_OUTPUT_FILE must be set in .env")
	}

	errorFolder := "files/errors"

	// Ensure directories exist
	folders := []string{"files/pass", "files/all", "files/done", "files/errors", "files/etbanks", "files/filter_errors", "files/txt"}
	for _, folder := range folders {
		if err := os.MkdirAll(folder, 0755); err != nil {
			return fmt.Errorf("creating folder %s: %w", folder, err)
		}
	}

	// Walk the directory provided in CONVERT_INPUT_DIR
	fileInfos, err := os.ReadDir(inputPath)
	if err != nil {
		return fmt.Errorf("reading folder %s: %w", inputPath, err)
	}

	for _, fileInfo := range fileInfos {
		if fileInfo.IsDir() {
			continue
		}
		filePath := filepath.Join(inputPath, fileInfo.Name())
		fmt.Println(fileInfo.Name())
		processFile(filePath, outputFile, errorFolder)
	}
	return nil
}
