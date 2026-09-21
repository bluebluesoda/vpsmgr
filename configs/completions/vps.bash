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
#   vps add|quota ... --<TAB> that command's flags
#   vps del|power|quota|passwd <TAB>   container names, from `vps list`
# Keys that `vps config set` refuses (immutable/auto/special) are not offered.

_vps() {
    local cur prev cword sub
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    cword=$COMP_CWORD
    sub="${COMP_WORDS[1]}"

    # Top-level commands (mirrors the dispatcher in main.go; the hidden
    # `completions` helper used by this script is deliberately not offered).
    local cmds="install serve panel-url add del list quota power passwd admin-passwd ipv6-reapply ipv6-proxy ip6 config transfer version"
    local add_flags="--cpu --mem --disk --bandwidth --days --extra64"
    local quota_flags="--cpu --mem --disk --bandwidth --days --clear-expiry --extra64"

    # A flag is being typed: offer the flags of the command in play. Checked
    # before the positional cases so it works at any depth, which is what makes
    # `vps quota <name> --<TAB>` find --extra64.
    if [[ "$cur" == -* ]]; then
        case "$sub" in
            add) COMPREPLY=( $(compgen -W "$add_flags" -- "$cur") ) ;;
            quota) COMPREPLY=( $(compgen -W "$quota_flags" -- "$cur") ) ;;
            config) [ "$cword" -ge 3 ] && COMPREPLY=( $(compgen -W "--apply --no-apply" -- "$cur") ) ;;
        esac
        return
    fi

    case "$cword" in
        1)
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
                transfer)
                    COMPREPLY=( $(compgen -W "send receive" -- "$cur") )
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

    # The value position of `vps config set <key>`: offer the apply variants
    # (empty cur included, so a bare TAB there still completes something).
    if [ "$sub" = "config" ] && [ "${COMP_WORDS[2]}" = "set" ] && [ "$cword" -gt 3 ]; then
        COMPREPLY=( $(compgen -W "--apply --no-apply" -- "$cur") )
    fi
}

complete -F _vps vps
