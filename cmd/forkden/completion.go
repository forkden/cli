package main

import (
	"fmt"
	"slices"
	"strings"
)

func allCommands() []commandHelp {
	result := []commandHelp{rootHelp()}
	for i := 0; i < len(result); i++ {
		result = append(result, result[i].children...)
	}
	return result
}

func completionWords(cmd commandHelp) string {
	words := []string{"--help", "--json", "--format", "--api-url", "--config-dir", "--socket"}
	for _, child := range cmd.children {
		words = append(words, commandName(child.path))
	}
	if cmd.path == "completion" {
		words = append(words, "bash", "zsh")
	}
	for _, word := range strings.Fields(cmd.usage + " " + cmd.flags) {
		if strings.HasPrefix(word, "--") {
			words = append(words, strings.TrimRight(word, "]|)"))
		}
	}
	slices.Sort(words)
	words = slices.Compact(words)
	return strings.Join(words, " ")
}

func completionScript(shell string) (string, error) {
	if shell != "bash" && shell != "zsh" {
		return "", invalidInput("supported completion shells: bash, zsh")
	}
	var script strings.Builder
	if shell == "zsh" {
		script.WriteString("#compdef forkden\n_forkden() {\n  local forkden_path='' forkden_word forkden_next forkden_candidates\n  local forkden_skip=0 forkden_i\n  for ((forkden_i=2; forkden_i<CURRENT; forkden_i++)); do\n    forkden_word=${words[forkden_i]}\n")
	} else {
		script.WriteString("# Forkden completion: source <(forkden completion bash)\n_forkden() {\n  local forkden_path='' forkden_word forkden_next forkden_candidates\n  local forkden_skip=0 forkden_i\n  for ((forkden_i=1; forkden_i<COMP_CWORD; forkden_i++)); do\n    forkden_word=${COMP_WORDS[forkden_i]}\n")
	}
	script.WriteString("    if ((forkden_skip)); then forkden_skip=0; continue; fi\n    case \"$forkden_word\" in\n      --) break ;;\n")
	valueNames := completionValueFlags()
	fmt.Fprintf(&script, "      %s) forkden_skip=1; continue ;;\n", strings.Join(valueNames, "|"))
	script.WriteString("      -*) continue ;;\n    esac\n    forkden_next=${forkden_path:+$forkden_path }$forkden_word\n    case \"$forkden_next\" in\n")
	paths := make([]string, 0, len(commands))
	for _, cmd := range allCommands()[1:] {
		paths = append(paths, "'"+cmd.path+"'")
	}
	fmt.Fprintf(&script, "      %s) forkden_path=$forkden_next ;;\n", strings.Join(paths, "|"))
	script.WriteString("    esac\n  done\n  case \"$forkden_path\" in\n")
	for _, cmd := range allCommands() {
		fmt.Fprintf(&script, "    '%s') forkden_candidates='%s' ;;\n", cmd.path, completionWords(cmd))
	}
	script.WriteString("  esac\n")
	if shell == "zsh" {
		script.WriteString("  ((forkden_skip)) && { _files; return; }\n  compadd -- ${(s: :)forkden_candidates}\n}\ncompdef _forkden forkden\n")
	} else {
		script.WriteString("  COMPREPLY=()\n  ((forkden_skip)) && return\n  while IFS= read -r forkden_word; do COMPREPLY+=(\"$forkden_word\"); done < <(compgen -W \"$forkden_candidates\" -- \"${COMP_WORDS[COMP_CWORD]}\")\n}\ncomplete -o default -F _forkden forkden\n")
	}
	return script.String(), nil
}

func completionValueFlags() []string {
	names := make([]string, 0, 2*len(valueFlags))
	for name := range valueFlags {
		names = append(names, "--"+name)
		if strings.Contains(name, "_") {
			names = append(names, "--"+strings.ReplaceAll(name, "_", "-"))
		}
	}
	slices.Sort(names)
	return names
}
