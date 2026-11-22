package actions

// ExportFromISO is fromISO for the tests.
func ExportFromISO(text string) (string, error) {
	moment, zoned, err := fromISO(text)
	if err == nil && zoned {
		return "", errISO
	}
	return isoSeconds(moment), err
}
