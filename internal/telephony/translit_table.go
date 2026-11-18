package telephony

// translitTable romanises Cyrillic (Russian, Ukrainian, Belarusian) and
// Greek letters, and the few Latin letters NFKD does not decompose. A
// capital's multi-letter romanisation capitalises its first letter only
// (Щ -> Shch). The hard and soft signs have no sound of their own and map to
// nothing. Accented Greek vowels (ά, ή …) are not listed: NFKD splits off the
// accent and the base letter is looked up here.
var translitTable = map[rune]string{
	// Cyrillic
	'А': "A", 'а': "a", 'Б': "B", 'б': "b", 'В': "V", 'в': "v", 'Г': "G", 'г': "g",
	'Д': "D", 'д': "d", 'Е': "E", 'е': "e", 'Ё': "E", 'ё': "e", 'Ж': "Zh", 'ж': "zh",
	'З': "Z", 'з': "z", 'И': "I", 'и': "i", 'Й': "Y", 'й': "y", 'К': "K", 'к': "k",
	'Л': "L", 'л': "l", 'М': "M", 'м': "m", 'Н': "N", 'н': "n", 'О': "O", 'о': "o",
	'П': "P", 'п': "p", 'Р': "R", 'р': "r", 'С': "S", 'с': "s", 'Т': "T", 'т': "t",
	'У': "U", 'у': "u", 'Ф': "F", 'ф': "f", 'Х': "Kh", 'х': "kh", 'Ц': "Ts", 'ц': "ts",
	'Ч': "Ch", 'ч': "ch", 'Ш': "Sh", 'ш': "sh", 'Щ': "Shch", 'щ': "shch",
	'Ъ': "", 'ъ': "", 'Ы': "Y", 'ы': "y", 'Ь': "", 'ь': "", 'Э': "E", 'э': "e",
	'Ю': "Yu", 'ю': "yu", 'Я': "Ya", 'я': "ya",
	'І': "I", 'і': "i", 'Ї': "Yi", 'ї': "yi", 'Є': "Ye", 'є': "ye", 'Ґ': "G", 'ґ': "g",
	'Ў': "U", 'ў': "u",

	// Greek
	'Α': "A", 'α': "a", 'Β': "V", 'β': "v", 'Γ': "G", 'γ': "g", 'Δ': "D", 'δ': "d",
	'Ε': "E", 'ε': "e", 'Ζ': "Z", 'ζ': "z", 'Η': "I", 'η': "i", 'Θ': "Th", 'θ': "th",
	'Ι': "I", 'ι': "i", 'Κ': "K", 'κ': "k", 'Λ': "L", 'λ': "l", 'Μ': "M", 'μ': "m",
	'Ν': "N", 'ν': "n", 'Ξ': "X", 'ξ': "x", 'Ο': "O", 'ο': "o", 'Π': "P", 'π': "p",
	'Ρ': "R", 'ρ': "r", 'Σ': "S", 'σ': "s", 'ς': "s", 'Τ': "T", 'τ': "t",
	'Υ': "Y", 'υ': "y", 'Φ': "F", 'φ': "f", 'Χ': "Ch", 'χ': "ch", 'Ψ': "Ps", 'ψ': "ps",
	'Ω': "O", 'ω': "o",

	// Latin letters without a canonical decomposition
	'ß': "ss", 'Æ': "AE", 'æ': "ae", 'Ø': "O", 'ø': "o", 'Œ': "OE", 'œ': "oe",
	'Đ': "D", 'đ': "d", 'Ł': "L", 'ł': "l", 'Þ': "Th", 'þ': "th", 'Ð': "D", 'ð': "d",
	'ı': "i",
}
