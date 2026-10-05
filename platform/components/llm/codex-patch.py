# LiteLLM decides streaming from the provider payload, so ChatGPT's mandatory stream flag is added only to the sent bytes (BerriAI/litellm#34094).
import json

from litellm.llms.chatgpt.responses.transformation import ChatGPTResponsesAPIConfig

transform = ChatGPTResponsesAPIConfig.transform_responses_api_request


def transform_responses_api_request(
    self, model, input, response_api_optional_request_params, litellm_params, headers
):
    request = transform(
        self,
        model,
        input,
        response_api_optional_request_params,
        litellm_params,
        headers,
    )
    request.pop("stream", None)
    if isinstance(request.get("input"), str):
        request["input"] = [
            {
                "type": "message",
                "role": "user",
                "content": [{"type": "input_text", "text": request["input"]}],
            }
        ]
    return request


def sign_request(
    self,
    headers,
    optional_params,
    request_data,
    api_base,
    api_key=None,
    model=None,
    stream=None,
    fake_stream=None,
):
    return headers, json.dumps({**request_data, "stream": True}).encode()


ChatGPTResponsesAPIConfig.transform_responses_api_request = (
    transform_responses_api_request
)
ChatGPTResponsesAPIConfig.sign_request = sign_request
