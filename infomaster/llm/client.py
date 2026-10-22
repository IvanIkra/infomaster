"""Claude Haiku with JSON-schema structured output, behind the daily limit and the journal (§7).

The SDK is imported on first use so tests and an idle service do not pay for it.
"""

import base64
import json
import logging
import os
import time
from collections.abc import Sequence
from dataclasses import dataclass

from .. import clock
from .budget import CHARS_PER_TOKEN, IMAGE_TOKENS, PAGE_TOKENS, Budget, cost_usd
from .journal import Journal

log = logging.getLogger(__name__)

MODEL = "claude-haiku-5-5"
TASKS = frozenset({"batch", "pdf", "image", "summary", "subjects", "grades", "hseapp_labels"})


class LLMError(RuntimeError):
    pass


class BadAnswer(LLMError):
    """The model answered, but not usably: a refusal, a cut answer, no text. The same input would fail again."""


class Rejected(LLMError):
    """The API refused the request as invalid (400). It may be the input (an image it cannot read) or the setup, so
    only a caller that knows which treats it as final."""


@dataclass(frozen=True)
class Document:
    """A PDF for the model to read, its pages as images and text: a scan has no other way in (§6.3)."""
    data: bytes
    pages: int


@dataclass(frozen=True)
class Image:
    """A picture for the model to look at (§5.1, р. 12): JPEG, PNG, GIF or WebP."""
    data: bytes
    media_type: str


class Haiku:
    def __init__(self, budget: Budget, journal: Journal, model: str = MODEL):
        self.budget, self.journal, self.model = budget, journal, model
        self._client = None

    @property
    def available(self) -> bool:
        return bool(os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("ANTHROPIC_AUTH_TOKEN"))

    async def json(self, task: str, system: str, prompt: str, schema: dict, *, max_tokens: int = 8000,
                   chat_id: int | None = None, documents: Sequence[Document] = (), images: Sequence[Image] = (),
                   asked: bool = False) -> dict:
        """One request whose answer must match `schema`; `documents`, then `images` go before the prompt, in order.
        `asked`: the owner asked for it, see Budget.reserve.

        Raises BudgetExceeded when the limit does not let the call happen (nothing is spent or logged then),
        LLMError when the call failed (it is logged with whatever tokens it spent), BadAnswer when the failure is
        in the answer itself rather than in the way to it.
        """
        if task not in TASKS:
            raise ValueError(f"unknown LLM task {task!r}")
        if not self.available:
            raise LLMError("ANTHROPIC_API_KEY is not set")
        price = self.budget.price(self.model)
        if price is None:
            raise LLMError(f"no price for {self.model} in the llm_prices setting")
        input_chars = len(system) + len(prompt) + len(json.dumps(schema)) + \
            (sum(d.pages for d in documents) * PAGE_TOKENS + len(images) * IMAGE_TOKENS) * CHARS_PER_TOKEN
        ticket = self.budget.reserve(task, self.budget.estimate(price, input_chars, max_tokens), asked)
        started = time.monotonic()
        response = None
        content = [*({"type": "document", "source": {"type": "base64", "media_type": "application/pdf",
                                                     "data": base64.standard_b64encode(d.data).decode()}}
                     for d in documents),
                   *({"type": "image", "source": {"type": "base64", "media_type": i.media_type,
                                                  "data": base64.standard_b64encode(i.data).decode()}}
                     for i in images), {"type": "text", "text": prompt}]
        try:
            response, text = await self._ask(system, content, schema, max_tokens)
            try:
                answer = json.loads(text)
            except ValueError as e:
                raise BadAnswer(f"the answer is not JSON: {e}") from e
        except Exception as e:
            response = getattr(e, "response", response)  # tokens are spent on a refusal or a cut answer too
            self._record(task, chat_id, price, started, response, str(e) or type(e).__name__)
            raise
        else:
            self._record(task, chat_id, price, started, response, None)
            return answer
        finally:
            self.budget.release(ticket)

    def _record(self, task: str, chat_id: int | None, price: dict, started: float, response,
                error: str | None) -> None:
        usage = getattr(response, "usage", None)
        tokens = {name: getattr(usage, name, None) or 0 for name in (
            "input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens")}
        run = {
            "at": clock.utc_iso(clock.now_utc()),
            "task": task,
            "chat_id": chat_id,
            "model": self.model,
            "ok": error is None,
            "error": error,
            "input_tokens": tokens["input_tokens"],
            "output_tokens": tokens["output_tokens"],
            "cache_write": tokens["cache_creation_input_tokens"],
            "cache_read": tokens["cache_read_input_tokens"],
            "cost_usd": cost_usd(price, tokens["input_tokens"], tokens["output_tokens"],
                                 tokens["cache_creation_input_tokens"], tokens["cache_read_input_tokens"]),
            "duration_s": round(time.monotonic() - started, 2),
        }
        try:
            self.journal.add(run)
        except Exception:
            log.exception("cannot record an LLM run")

    def _get_client(self):
        if self._client is None:
            import anthropic

            self._client = anthropic.AsyncAnthropic(timeout=120.0, max_retries=3)
        return self._client

    async def _ask(self, system: str, content: list[dict], schema: dict, max_tokens: int):
        import anthropic

        try:
            response = await self._get_client().messages.create(
                model=self.model,
                max_tokens=max_tokens,
                system=system,
                messages=[{"role": "user", "content": content}],
                output_config={"effort": "low", "format": {"type": "json_schema", "schema": schema}},
            )
        except anthropic.AuthenticationError as e:
            raise LLMError("Anthropic API key was rejected") from e
        except anthropic.RateLimitError as e:
            raise LLMError("Anthropic rate limit") from e
        except anthropic.BadRequestError as e:
            raise Rejected(f"Anthropic API error {e.status_code}: {e.message}") from e
        except anthropic.APIStatusError as e:
            raise LLMError(f"Anthropic API error {e.status_code}: {e.message}") from e
        except anthropic.APIConnectionError as e:
            raise LLMError(f"cannot reach the Anthropic API: {e}") from e

        log.info("haiku: %d in / %d out tokens", response.usage.input_tokens, response.usage.output_tokens)
        problem = None
        if response.stop_reason == "refusal":
            problem = f"request declined ({getattr(response.stop_details, 'category', None)})"
        elif response.stop_reason == "max_tokens":
            problem = "answer was cut at max_tokens"
        text = next((b.text for b in response.content if b.type == "text"), None)
        if problem is None and text is None:
            problem = "no text in the answer"
        if problem:
            error = BadAnswer(problem)
            error.response = response
            raise error
        return response, text
