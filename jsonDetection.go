package main

func removeWhitespace(b *[]byte) bool {
	for len(*b) > 0 {
		if (*b)[0] == ' ' || (*b)[0] == '\t' || (*b)[0] == '\n' || (*b)[0] == '\r' {
			*b = (*b)[1:]
		} else {
			return true
		}
	}
	return true
}

func null_(b *[]byte) bool {
	if len(*b) < 3 {
		return true
	}
	if (*b)[0] != 'u' || (*b)[1] != 'l' || (*b)[2] != 'l' {
		return false
	}
	*b = (*b)[3:]
	return true
}

func true_(b *[]byte) bool {
	if len(*b) < 3 {
		return true
	}
	if (*b)[0] != 'r' || (*b)[1] != 'u' || (*b)[2] != 'e' {
		return false
	}
	*b = (*b)[3:]
	return true
}

func false_(b *[]byte) bool {
	if len(*b) < 4 {
		return true
	}
	if (*b)[0] != 'a' || (*b)[1] != 'l' || (*b)[2] != 's' || (*b)[3] != 'e' {
		return false
	}
	*b = (*b)[4:]
	return true
}

func string_(b *[]byte) bool {
	if len(*b) == 0 {
		return true
	}
	if (*b)[0] != '"' {
		return false
	}
	*b = (*b)[1:]
	for len(*b) > 0 {
		if (*b)[0] == '"' {
			*b = (*b)[1:]
			return true
		}
		if (*b)[0] == '\\' {
			*b = (*b)[1:]
			if len(*b) == 0 {
				return true
			}
		}
		*b = (*b)[1:]
	}
	return true
}

func object(b *[]byte) bool {
	ok := removeWhitespace(b)
	if !ok {
		return false
	}
	if len(*b) == 0 {
		return true
	}
	if (*b)[0] == '}' {
		*b = (*b)[1:]
		return true
	}
	for len(*b) > 0 {
		if !string_(b) {
			return false
		}
		ok = removeWhitespace(b)
		if !ok {
			return false
		}
		if len(*b) == 0 {
			return true
		}
		if (*b)[0] != ':' {
			return false
		}
		*b = (*b)[1:]
		if !json(b) {
			return false
		}
		ok = removeWhitespace(b)
		if !ok {
			return false
		}
		if len(*b) == 0 {
			return true
		}
		if (*b)[0] == '}' {
			*b = (*b)[1:]
			return true
		}
		if (*b)[0] != ',' {
			return false
		}
		*b = (*b)[1:]
	}
	return true
}

func array(b *[]byte) bool {
	ok := removeWhitespace(b)
	if !ok {
		return false
	}
	if len(*b) == 0 {
		return true
	}
	if (*b)[0] == ']' {
		*b = (*b)[1:]
		return true
	}
	for len(*b) > 0 {
		if !json(b) {
			return false
		}
		ok = removeWhitespace(b)
		if !ok {
			return false
		}
		if len(*b) == 0 {
			return true
		}
		if (*b)[0] == ']' {
			*b = (*b)[1:]
			return true
		}
		if (*b)[0] != ',' {
			return false
		}
		*b = (*b)[1:]
	}
	return true
}

func json(b *[]byte) bool {
	if len(*b) == 0 {
		return false
	}
	ok := removeWhitespace(b)
	if !ok {
		return false
	}
	switch (*b)[0] {
	case '{':
		*b = (*b)[1:]
		return object(b)
	case '[':
		*b = (*b)[1:]
		return array(b)
	case '"':
		return string_(b)
	case 't':
		*b = (*b)[1:]
		return true_(b)
	case 'f':
		*b = (*b)[1:]
		return false_(b)
	case 'n':
		*b = (*b)[1:]
		return null_(b)
	}
	return false
}
