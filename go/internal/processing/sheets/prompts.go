package sheets

// Code generated from the Python prompts and schemas, verbatim; do not edit by hand.

import "encoding/json"

// ReadSystem is the system prompt of reading a sheet; {language} and {no_intro} are filled by the app's language.
const ReadSystem = "You read a sheet (листок) of a university course: a PDF with tasks for students (HSE University, Moscow, first year, Software Engineering). The student sees your summary in Telegram under the file.\n" +
	"\n" +
	"Output:\n" +
	"- summary: in {language}, at most 3 short sentences: what the tasks are about, how many there are, how and when to hand in, anything unusual (a defence, a variant to choose, bonus tasks). No greeting, no \"{no_intro}\".\n" +
	"- deadlines: every date the student must hand something in by, as written in the sheet: label \"<abbr> <work>\" in English within 14 characters with the course abbreviation given (\"Calc HW 3\", \"DM ИДЗ-1\" → \"DM IDZ-1\"), due YYYY-MM-DD; a date without a year is in the academic year of the posting date. [] when the sheet gives no date.\n"

// ChangesSystem is the system prompt of describing a newer version; {language} and {layout_only} are filled.
const ChangesSystem = "You compare two versions of a sheet (листок) of a university course: a PDF with tasks for students. You get only the lines that changed: \"-\" the old version, \"+\" the new one, as text extracted from the PDF (layout noise is possible).\n" +
	"\n" +
	"Output changes: in {language}, one or two short sentences on what changed for the student: a task fixed or added, a new due date, a typo. Ignore changes of layout or spacing. \"{layout_only}\" when nothing else did.\n"

// ReadSchema is the schema of reading a sheet.
var ReadSchema = json.RawMessage("{\"type\": \"object\", \"properties\": {\"summary\": {\"type\": \"string\"}, \"deadlines\": {\"type\": \"array\", \"items\": {\"type\": \"object\", \"properties\": {\"label\": {\"type\": \"string\"}, \"due\": {\"type\": \"string\"}}, \"required\": [\"label\", \"due\"], \"additionalProperties\": false}}}, \"required\": [\"summary\", \"deadlines\"], \"additionalProperties\": false}")

// ChangesSchema is the schema of describing a newer version.
var ChangesSchema = json.RawMessage("{\"type\": \"object\", \"properties\": {\"changes\": {\"type\": \"string\"}}, \"required\": [\"changes\"], \"additionalProperties\": false}")
