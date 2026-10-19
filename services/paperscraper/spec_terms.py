"""Source-neutral helpers for the structured search spec sent by the Go application."""

from __future__ import annotations

from typing import Any


def terms_from_spec(spec: dict[str, Any]) -> list[str | list[str]]:
    """Outer list is AND across concepts; an inner list is OR across one concept's synonyms."""
    groups: list[str | list[str]] = []
    for item in spec.get("concepts") or []:
        terms = [str(x).strip() for x in (item.get("synonyms") or []) if str(x).strip()]
        if not terms and str(item.get("concept") or "").strip():
            terms = [str(item["concept"]).strip()]
        if len(terms) == 1:
            groups.append(terms[0])
        elif terms:
            groups.append(terms)
    if not groups:
        raise ValueError("search spec has no concepts")
    return groups


def matches_exclusions(record: dict[str, Any], excluded: list[str]) -> bool:
    """True when the record contains none of the excluded terms in its title or abstract."""
    haystack = f"{record.get('title') or ''} {record.get('abstract') or ''}".lower()
    return not any(term.strip().lower() in haystack for term in excluded if term.strip())


def excluded_identities(payload: dict[str, Any]) -> set[tuple[str, str]]:
    return {
        (str(item.get("source") or "").strip().lower(), str(item.get("externalId") or "").strip())
        for item in (payload.get("exclude") or [])
        if isinstance(item, dict)
    }
