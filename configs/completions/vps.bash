# vpsmgr bash completion for the `vps` CLI.
# Generated/installed by scripts/40-panel.sh into
# /usr/share/bash-completion/completions/vps and sourced automatically by the
# bash-completion package on the next shell. Reload with:
#     source /usr/share/bash-completion/completions/vps
#
# Completion is context aware:
#   vps <TAB>                 top-level commands
#   vps config <TAB>          config subcommands (set/list/help)
#   vps config set <TAB>      config keys accepted by `vps config set`, read
#                             live from `vps config completions` so they always
#                             match the registry (never a stale list).
# Keys that `vps config set` refuses (immutable/auto/special) are not offered.

_vps() {
    local cur prev cword
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    cword=$COMP_CWORD

    local sub="${COMP_WORDS[1]}"

    case "$cword" in
        1)
            # Top-level commands (mirrors the dispatcher in main.go).
            local cmds="install serve panel-url add del list quota power passwd admin-passwd ipv6-reapply ip6 config version"
            COMPREPLY=( $(compgen -W "$cmds" -- "$cur") )
            return
            ;;
        2)
            case "$sub" in
                config)
                    COMPREPLY=( $(compgen -W "set list help" -- "$cur") )
                    return
                    ;;
                del|power|quota|passwd)
                    # Container names: complete from `vps list` (names only).
                    local names
                    names=$(vps list 2>/dev/null | awk 'NR>1{print $1}')
                    COMPREPLY=( $(compgen -W "$names" -- "$cur") )
                    return
                    ;;
                *)
                    return
                    ;;
            esac
            ;;
        3)
            # `vps config set <key>`: complete the key from the live registry.
            if [ "$sub" = "config" ] && [ "${COMP_WORDS[2]}" = "set" ]; then
                local keys
                keys=$(vps config completions 2>/dev/null)
                COMPREPLY=( $(compgen -W "$keys" -- "$cur") )
                return
            fi
            ;;
    esac

    # Everything past the key position (the value / flags) is free-form —
    # offer the common apply variants as a convenience.
    if [ "$sub" = "config" ] && [ "${COMP_WORDS[2]}" = "set" ] && [ "$cword" -gt 3 ]; then
        COMPREPLY=( $(compgen -W "--apply --no-apply" -- "$cur") )
    fi
}

complete -F _vps vps