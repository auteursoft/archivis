#!/usr/bin/env python3
"""Reference CLIP tokenizer: OpenAI's simple_tokenizer.py, including its ftfy
text fixing. Generates and checks internal/ml/testdata/tok_ref.json.

    clip_tokenizer_ref.py internal/ml/assets/bpe_simple_vocab_16e6.txt.gz "text" ...
    clip_tokenizer_ref.py VOCAB --json "text" ...     # tok_ref.json entries
    clip_tokenizer_ref.py VOCAB --check tok_ref.json  # verify existing entries

Needs: pip install ftfy regex
"""
import gzip
import html
import json
import sys
from functools import lru_cache

import ftfy
import regex as re


@lru_cache()
def bytes_to_unicode():
    bs = list(range(ord("!"), ord("~") + 1)) + list(range(ord("¡"), ord("¬") + 1)) + list(range(ord("®"), ord("ÿ") + 1))
    cs = bs[:]
    n = 0
    for b in range(2 ** 8):
        if b not in bs:
            bs.append(b)
            cs.append(2 ** 8 + n)
            n += 1
    return dict(zip(bs, [chr(c) for c in cs]))


def get_pairs(word):
    return {(a, b) for a, b in zip(word, word[1:])}


def basic_clean(text):
    text = ftfy.fix_text(text)
    text = html.unescape(html.unescape(text))
    return text.strip()


def whitespace_clean(text):
    return re.sub(r"\s+", " ", text).strip()


class SimpleTokenizer:
    def __init__(self, bpe_path):
        self.byte_encoder = bytes_to_unicode()
        merges = gzip.open(bpe_path).read().decode("utf-8").split("\n")
        merges = [tuple(m.split()) for m in merges[1:49152 - 256 - 2 + 1]]
        vocab = list(bytes_to_unicode().values())
        vocab = vocab + [v + "</w>" for v in vocab]
        vocab.extend("".join(m) for m in merges)
        vocab.extend(["<|startoftext|>", "<|endoftext|>"])
        self.encoder = dict(zip(vocab, range(len(vocab))))
        self.bpe_ranks = dict(zip(merges, range(len(merges))))
        self.cache = {"<|startoftext|>": "<|startoftext|>", "<|endoftext|>": "<|endoftext|>"}
        self.pat = re.compile(r"""<\|startoftext\|>|<\|endoftext\|>|'s|'t|'re|'ve|'m|'ll|'d|[\p{L}]+|[\p{N}]|[^\s\p{L}\p{N}]+""", re.IGNORECASE)

    def bpe(self, token):
        if token in self.cache:
            return self.cache[token]
        word = tuple(token[:-1]) + (token[-1] + "</w>",)
        pairs = get_pairs(word)
        if not pairs:
            return token + "</w>"
        while True:
            bigram = min(pairs, key=lambda p: self.bpe_ranks.get(p, float("inf")))
            if bigram not in self.bpe_ranks:
                break
            first, second = bigram
            new_word, i = [], 0
            while i < len(word):
                try:
                    j = word.index(first, i)
                    new_word.extend(word[i:j])
                    i = j
                except ValueError:
                    new_word.extend(word[i:])
                    break
                if word[i] == first and i < len(word) - 1 and word[i + 1] == second:
                    new_word.append(first + second)
                    i += 2
                else:
                    new_word.append(word[i])
                    i += 1
            word = tuple(new_word)
            if len(word) == 1:
                break
            pairs = get_pairs(word)
        word = " ".join(word)
        self.cache[token] = word
        return word

    def encode(self, text):
        ids = []
        text = whitespace_clean(basic_clean(text)).lower()
        for token in re.findall(self.pat, text):
            token = "".join(self.byte_encoder[b] for b in token.encode("utf-8"))
            ids.extend(self.encoder[t] for t in self.bpe(token).split(" "))
        return ids


if __name__ == "__main__":
    tok = SimpleTokenizer(sys.argv[1])
    args = sys.argv[2:]
    if args[:1] == ["--check"]:
        ref = json.load(open(args[1]))
        bad = [r["text"] for r in ref if tok.encode(r["text"]) != r["ids"]]
        print(f"{len(ref) - len(bad)}/{len(ref)} reference cases reproduced", bad)
        sys.exit(1 if bad else 0)
    if args[:1] == ["--json"]:
        print(json.dumps([{"text": t, "ids": tok.encode(t)} for t in args[1:]], ensure_ascii=True))
    else:
        for t in args:
            print(t, tok.encode(t))
