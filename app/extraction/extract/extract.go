package extract

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/nwaples/rardecode"
	"github.com/yeka/zip"
)

func ExtractArchives() {
	fmt.Print("\033[H\033[2J")
	color.Cyan("\nStarting the EXTRACTOR...\n")

	inputDirectory := "app/extraction/files/all"
	outputDirectory := "app/extraction/files/pass"

	processArchivesInDir(inputDirectory, outputDirectory)
}

func readPasswordsFromFile(passwordFile string) []string {
	passwordsList := []string{""}

	if _, err := os.Stat(passwordFile); os.IsNotExist(err) {
		color.Red("🚫 Password 📂 file %s does not exist.", passwordFile)
		color.Yellow("⚠️ Trying to extract without a password!")
		return passwordsList
	}

	file, err := os.Open(passwordFile)
	if err != nil {
		color.Red("🛠 An error occurred while 🔄 reading the password file: %v", err)
		return passwordsList
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		password := strings.TrimSpace(scanner.Text())
		if password != "" {
			passwordsList = append(passwordsList, password)
		}
	}

	if err := scanner.Err(); err != nil {
		color.Red("🛠 An error occurred while 🔄 reading the password file: %v", err)
	}

	return passwordsList
}

func extractZIPFiles(archivePath, destinationPath string, passwords []string) (bool, bool, bool) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		color.Red("🛠️ Error opening ZIP file: %v", err)
		return false, false, true // extraction failed, not password issue, should delete
	}
	defer r.Close()

	passwordProtectedFiles := 0
	hasPasswordFiles := false
	for _, f := range r.File {
		match, _ := regexp.MatchString(`.*asswor.*\.txt`, f.Name)
		if match {
			passwordProtectedFiles++
			if f.IsEncrypted() {
				hasPasswordFiles = true
			}
		}
	}

	color.Yellow("💡 Found %d 🔑 Password-protected files\n", passwordProtectedFiles)

	extractedFiles := 0
	passwordFailed := false

	for _, f := range r.File {
		match, _ := regexp.MatchString(`.*asswor.*\.txt`, f.Name)
		if !match {
			continue
		}

		fileExtracted := false
		for _, password := range passwords {
			if f.IsEncrypted() {
				f.SetPassword(password)
			}

			rc, err := f.Open()
			if err != nil {
				continue
			}

			// Read content into memory first to verify extraction works
			content, err := io.ReadAll(rc)
			rc.Close()

			if err != nil {
				continue
			}

			// Only create file if extraction was successful
			timestamp := time.Now().UnixNano()
			newFilename := fmt.Sprintf("password_%d_%d.txt", extractedFiles, timestamp)
			newFilePath := filepath.Join(destinationPath, newFilename)

			outFile, err := os.Create(newFilePath)
			if err != nil {
				color.Red("🛠️ Error creating file: %v", err)
				continue
			}

			_, err = outFile.Write(content)
			outFile.Close()

			if err != nil {
				color.Red("🛠️ Error writing file: %v", err)
				os.Remove(newFilePath) // Clean up failed file
				continue
			}

			color.Green("✅ File saved: %s", newFilePath)
			extractedFiles++
			fileExtracted = true
			break // Move to the next file after successful extraction
		}

		if !fileExtracted && f.IsEncrypted() {
			passwordFailed = true
		}
	}

	if extractedFiles > 0 {
		return true, false, false // success
	} else if hasPasswordFiles && passwordFailed {
		return false, true, false // password failed, move to nopass
	} else {
		return false, false, true // no files extracted, delete
	}
}

func extractRARFiles(archivePath, destinationPath string, passwords []string) (bool, bool, bool) {
	passwordProtectedFiles := 0
	extractedFiles := 0
	hasPasswordFiles := false
	isArchivePasswordProtected := false

	// First, try to open without password and attempt to read to detect if archive is password-protected
	rr, err := rardecode.OpenReader(archivePath, "")
	if err != nil {
		// Archive is likely password-protected
		isArchivePasswordProtected = true
		color.Yellow("🔒 Archive is password-protected, trying passwords...")
	} else {
		// Try to read the first file to check if archive is actually password-protected
		canReadFiles := false
		for {
			_, err := rr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				// Error reading files - likely password-protected
				isArchivePasswordProtected = true
				break
			}

			// Try to read a small amount to test if we can actually access the content
			testBuffer := make([]byte, 1)
			_, err = rr.Read(testBuffer)
			if err != nil && err != io.EOF {
				// Can't read content - likely password-protected
				isArchivePasswordProtected = true
				break
			}

			canReadFiles = true
			break // We only need to test one file
		}
		rr.Close()

		if isArchivePasswordProtected {
			color.Yellow("🔒 Archive is password-protected, trying passwords...")
		} else if canReadFiles {
			// Archive is not password-protected, try to extract
			color.Yellow("🔓 Archive is not password-protected, extracting directly...")
			rr, err = rardecode.OpenReader(archivePath, "")
			if err != nil {
				return false, false, true
			}

			for {
				header, err := rr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					break
				}

				match, _ := regexp.MatchString(`.*asswor.*\.txt`, header.Name)
				if !match {
					continue
				}

				hasPasswordFiles = true

				// Read content into memory first to verify extraction works
				content, err := io.ReadAll(rr)
				if err != nil {
					continue
				}

				// Only create file if extraction was successful
				timestamp := time.Now().UnixNano()
				newFilename := fmt.Sprintf("password_%d_%d.txt", extractedFiles, timestamp)
				newFilePath := filepath.Join(destinationPath, newFilename)

				outFile, err := os.Create(newFilePath)
				if err != nil {
					color.Red("🛠️ Error creating file: %v", err)
					continue
				}

				_, err = outFile.Write(content)
				outFile.Close()

				if err != nil {
					color.Red("🛠️ Error writing file: %v", err)
					os.Remove(newFilePath) // Clean up failed file
					continue
				}

				color.Green("✅ File saved: %s", newFilePath)
				extractedFiles++
			}
			rr.Close()
		} else {
			// Archive opened but has no files - treat as unextractable
			isArchivePasswordProtected = false
		}
	}

	// If archive is password-protected, try each password
	if isArchivePasswordProtected {
		for _, password := range passwords {
			rr, err := rardecode.OpenReader(archivePath, password)
			if err != nil {
				continue
			}

			for {
				header, err := rr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					// If it's any error, try the next password
					break
				}

				match, _ := regexp.MatchString(`.*asswor.*\.txt`, header.Name)
				if !match {
					continue
				}

				hasPasswordFiles = true
				passwordProtectedFiles++

				// Read content into memory first to verify extraction works
				content, err := io.ReadAll(rr)
				if err != nil {
					continue
				}

				// Only create file if extraction was successful
				timestamp := time.Now().UnixNano()
				newFilename := fmt.Sprintf("password_%d_%d.txt", extractedFiles, timestamp)
				newFilePath := filepath.Join(destinationPath, newFilename)

				outFile, err := os.Create(newFilePath)
				if err != nil {
					color.Red("🛠️ Error creating file: %v", err)
					continue
				}

				_, err = outFile.Write(content)
				outFile.Close()

				if err != nil {
					color.Red("🛠️ Error writing file: %v", err)
					os.Remove(newFilePath) // Clean up failed file
					continue
				}

				color.Green("✅ File saved: %s", newFilePath)
				extractedFiles++
			}
			rr.Close()

			if extractedFiles > 0 {
				break // Stop trying passwords if files were extracted
			}
		}
	}

	if hasPasswordFiles {
		color.Yellow("💡 Found %d files matching password pattern\n", passwordProtectedFiles)
	} else if isArchivePasswordProtected {
		color.Yellow("💡 Archive is password-protected but contains no password files matching pattern\n")
	} else {
		color.Yellow("💡 Archive is not password-protected and contains no password files\n")
	}

	if extractedFiles > 0 {
		return true, false, false // success
	} else if isArchivePasswordProtected {
		return false, true, false // password-protected archive, move to nopass
	} else {
		return false, false, true // no files extracted, delete
	}
}

func generateUniqueFilename(dir, filename string) string {
	originalPath := filepath.Join(dir, filename)

	// If file doesn't exist, return original filename
	if _, err := os.Stat(originalPath); os.IsNotExist(err) {
		return filename
	}

	// File exists, generate new filename with timestamp prefix
	now := time.Now()
	timestamp := now.Format("20060102_150405")

	// Extract file extension
	ext := filepath.Ext(filename)
	name := strings.TrimSuffix(filename, ext)

	// Create new filename with timestamp prefix
	newFilename := fmt.Sprintf("%s_%s%s", timestamp, name, ext)
	return newFilename
}

func forceDeleteFile(filePath string) error {
	maxAttempts := 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := os.Remove(filePath)
		if err == nil {
			return nil
		}

		color.Yellow("Attempt %d to delete file failed: %v", attempt+1, err)

		// Force garbage collection to release file handles
		runtime.GC()

		// Wait a bit before the next attempt
		time.Sleep(time.Second)
	}
	return fmt.Errorf("failed to delete file after %d attempts", maxAttempts)
}

func processArchivesInDir(inputDir, outputDir string) {
	if _, err := os.Stat(inputDir); os.IsNotExist(err) {
		color.Red("🚫 Input directory %s does not exist.", inputDir)
		return
	}

	if _, err := os.Stat(outputDir); os.IsNotExist(err) {
		os.MkdirAll(outputDir, os.ModePerm)
		color.Yellow("⚠️ Output directory %s created.", outputDir)
	}

	// Create nopass directory if it doesn't exist
	nopassDir := "files/nopass"
	if _, err := os.Stat(nopassDir); os.IsNotExist(err) {
		os.MkdirAll(nopassDir, os.ModePerm)
		color.Yellow("⚠️ No-password directory %s created.", nopassDir)
	}

	start := time.Now()

	passwords := readPasswordsFromFile("./pass.txt")

	for {
		files, err := os.ReadDir(inputDir)
		if err != nil {
			color.Red("🛠️ Error reading directory: %v", err)
			return
		}

		supportedFiles := 0
		processedFiles := 0

		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".zip") || strings.HasSuffix(file.Name(), ".rar") {
				supportedFiles++
			}
		}

		if supportedFiles == 0 {
			break
		}

		color.Cyan("📂 Processing %d supported files in %s", supportedFiles, inputDir)

		for _, file := range files {
			filePath := filepath.Join(inputDir, file.Name())
			var success, passwordFailed, shouldDelete bool
			if strings.HasSuffix(file.Name(), ".zip") {
				color.Blue("\n📦 Found ZIP archive: %s", filePath)
				success, passwordFailed, shouldDelete = extractZIPFiles(filePath, outputDir, passwords)
			} else if strings.HasSuffix(file.Name(), ".rar") {
				color.Blue("\n📦 Found RAR archive: %s", filePath)
				success, passwordFailed, shouldDelete = extractRARFiles(filePath, outputDir, passwords)
			} else {
				continue
			}

			if success {
				// Successfully extracted, delete the archive
				err := forceDeleteFile(filePath)
				if err != nil {
					color.Red("🛠️ Error deleting file: %v", err)
					// If deletion failed, rename the file to prevent re-processing
					newPath := filePath + ".processed"
					if renameErr := os.Rename(filePath, newPath); renameErr != nil {
						color.Red("❌ Failed to rename file: %v", renameErr)
					} else {
						color.Yellow("⚠️ Renamed file to: %s", newPath)
					}
				} else {
					color.Green("🗑️ Deleted archive file: %s", filePath)
					processedFiles++
				}
			} else if passwordFailed {
				// Password protected but no correct password found, move to nopass
				uniqueFilename := generateUniqueFilename(nopassDir, file.Name())
				nopassPath := filepath.Join(nopassDir, uniqueFilename)
				err := os.Rename(filePath, nopassPath)
				if err != nil {
					color.Red("🛠️ Error moving file to nopass: %v", err)
				} else {
					color.Yellow("🔒 Moved password-protected file to: %s", nopassPath)
					processedFiles++
				}
			} else if shouldDelete {
				// Archive couldn't be extracted by any means, delete it
				err := forceDeleteFile(filePath)
				if err != nil {
					color.Red("🛠️ Error deleting unextractable file: %v", err)
					// If deletion failed, rename the file to prevent re-processing
					newPath := filePath + ".failed"
					if renameErr := os.Rename(filePath, newPath); renameErr != nil {
						color.Red("❌ Failed to rename failed file: %v", renameErr)
					} else {
						color.Yellow("⚠️ Renamed failed file to: %s", newPath)
					}
				} else {
					color.Red("🗑️ Deleted unextractable archive: %s", filePath)
					processedFiles++
				}
			}
		}

		color.Yellow("Processed %d out of %d supported files", processedFiles, supportedFiles)
	}

	elapsed := time.Since(start)
	color.Green("Total Extraction Time: %s", elapsed)
}
