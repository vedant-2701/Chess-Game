#!/usr/bin/env bash
set -euo pipefail

RED='\033[0;31m' GREEN='\033[0;32m' YELLOW='\033[1;33m' NC='\033[0m'

[[ -f .env && -f .env.example ]] || { echo -e "${RED}✖ Error: .env or .env.example missing in current folder.${NC}"; exit 1; }

# Helper: Extract keys strictly ignoring comments/blanks (supports letters, numbers, _, -)
get_keys() { grep -E '^[a-zA-Z0-9_-]+=' "$1" | cut -d= -f1 | sort -u; }

mapfile -t missing < <(comm -23 <(get_keys .env.example) <(get_keys .env))
mapfile -t empty < <(grep -E '^[a-zA-Z0-9_-]+=\s*($|#)' .env | cut -d= -f1 || true)

# Remove any empty string artifacts if arrays are blank
missing=(${missing[@]:-})
empty=(${empty[@]:-})

if [[ ${#missing[@]} -eq 0 && ${#empty[@]} -eq 0 ]]; then
  echo -e "${GREEN}✔ Everything matches perfectly! All keys are present and assigned.${NC}"
  exit 0
fi

if [[ ${#missing[@]} -gt 0 ]]; then
  echo -e "${RED}✖ Keys present in .env.example but completely missing from .env:${NC}"
  printf "${RED}  - %s${NC}\n" "${missing[@]}"
fi

if [[ ${#empty[@]} -gt 0 ]]; then
  echo -e "${YELLOW}⚠ Keys present in .env but have no value assigned:${NC}"
  printf "${YELLOW}  - %s${NC}\n" "${empty[@]}"
fi

if [[ ${#missing[@]} -gt 0 ]]; then
  echo ""
  read -p "Would you like to append these missing keys to your .env file? (y/n) " -r answer
  if [[ "$answer" =~ ^[Yy]$ ]]; then
    # Ensure file ends with a newline before appending
    [[ $(tail -c1 .env | wc -l) -eq 0 ]] && echo "" >> .env
    for key in "${missing[@]}"; do echo "$key=" >> .env; done
    echo -e "${GREEN}✔ Missing keys safely appended to .env.${NC}"
  fi
fi
