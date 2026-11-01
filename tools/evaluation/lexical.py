"""한국어 FTS5 tokenizer 후보와 색인.

한국어 FTS tokenizer 비교 코드다.
한국어는 교착어라 조사가 붙으면(`초코우유를`) 기본 tokenizer가 어간(`초코우유`)을
찾지 못한다. 형태소 분석기 없이 SQLite만으로 해결 가능한지 비교한다.
"""
from __future__ import annotations

import re
import sqlite3
import unicodedata

_HANGUL = re.compile(r"[가-힣]")
# 한글 음절 구간 또는 공백이 아닌 문자 하나. str.isspace()와 \s는 같은 문자 집합이다.
_RUNS = re.compile(r"[가-힣]+|\S")


def _is_hangul(ch: str) -> bool:
    return bool(_HANGUL.match(ch))


def to_ngrams(text: str, n: int) -> str:
    """한글 구간만 n-gram으로 펼치고 나머지는 그대로 둔다.

    `초코우유를` -> (n=2) `초코 코우 우유 유를`
    질의 `초코우유` -> `초코 코우 우유` 이므로 AND 매칭으로 부분일치가 성립한다.
    한글이 아닌 문자는 공백을 빼고 한 글자씩 토큰이 된다. 색인 구축 시간의 대부분이라 정규식으로 구간을 자른다.
    """
    out: list[str] = []
    for m in _RUNS.finditer(unicodedata.normalize("NFKC", text)):
        s = m.group()
        if "가" <= s[0] <= "힣" and len(s) >= n:
            out.extend([s[i : i + n] for i in range(len(s) - n + 1)])
        else:
            out.append(s)
    return " ".join(out)


_PUNCT = re.compile(r"[^\w가-힣]+", re.UNICODE)
# 질문 어미·조사만 남는 토큰은 변별력이 없어 질의에서 뺀다.
_STOP = frozenset({"뭐", "무엇", "어디", "언제", "누구", "누가", "왜", "어떻게", "얼마",
                   "인가요", "인가", "한가요", "했나요", "하나요", "무슨", "어떤", "그리고"})


def query_words(q: str) -> list[str]:
    """질의를 변별력 있는 단어로 자른다."""
    words = [w for w in _PUNCT.sub(" ", unicodedata.normalize("NFKC", q)).split() if w]
    return [w for w in words if len(w) >= 2 and w not in _STOP]


def _or_phrases(q: str, n: int | None) -> str:
    """단어별 phrase를 OR로 묶는다.

    질문 전체를 AND로 묶으면 모든 토큰을 포함하는 문서가 없어 0건이 된다.
    단어 안에서는 인접(phrase), 단어 사이는 OR로 두고 순위는 bm25에 맡긴다.
    """
    parts: list[str] = []
    for word in query_words(q):
        grams = to_ngrams(word, n).split() if n else [word]
        if grams:
            parts.append('"' + " ".join(g.replace('"', '""') for g in grams) + '"')
    return " OR ".join(parts)


# tokenizer 후보. build(text)는 색인 문자열, query(q)는 MATCH 식을 만든다.
VARIANTS: dict[str, dict] = {
    "unicode61": {
        "tokenize": "unicode61",
        "build": lambda t: t,
        "query": lambda q: _or_phrases(q, None),
    },
    "trigram": {
        "tokenize": "trigram",
        "build": lambda t: t,
        "query": lambda q: _or_phrases(q, None),
    },
    "bigram": {
        "tokenize": "unicode61",
        "build": lambda t: to_ngrams(t, 2),
        "query": lambda q: _or_phrases(q, 2),
    },
    "trigram_expand": {
        "tokenize": "unicode61",
        "build": lambda t: to_ngrams(t, 3),
        "query": lambda q: _or_phrases(q, 3),
    },
}


class LexicalIndex:
    """단일 tokenizer 변형에 대한 in-memory FTS5 색인."""

    def __init__(self, variant: str) -> None:
        if variant not in VARIANTS:
            raise ValueError(f"알 수 없는 tokenizer 변형: {variant}")
        self.variant = variant
        self.spec = VARIANTS[variant]
        self.db = sqlite3.connect(":memory:")
        self.db.execute(
            f"CREATE VIRTUAL TABLE docs USING fts5(doc_id UNINDEXED, body, "
            f"tokenize='{self.spec['tokenize']}')"
        )

    def close(self) -> None:
        self.db.close()

    def __del__(self):
        db = getattr(self, "db", None)
        if db is not None:
            db.close()

    def add_many(self, rows: list[tuple[str, str]]) -> None:
        """(doc_id, 원문) 목록을 색인한다."""
        build = self.spec["build"]
        self.db.executemany(
            "INSERT INTO docs(doc_id, body) VALUES (?, ?)",
            [(doc_id, build(text)) for doc_id, text in rows],
        )
        self.db.commit()

    def search(self, query: str, limit: int = 10) -> list[tuple[str, float]]:
        expr = self.spec["query"](query)
        if not expr:
            return []
        try:
            cur = self.db.execute(
                "SELECT doc_id, bm25(docs) FROM docs WHERE docs MATCH ? "
                "ORDER BY bm25(docs) LIMIT ?",
                (expr, limit),
            )
            return [(r[0], r[1]) for r in cur]
        except sqlite3.OperationalError:
            return []
