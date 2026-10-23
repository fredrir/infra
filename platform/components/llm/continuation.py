from litellm.integrations.custom_logger import CustomLogger


class FinalMessageContinuation(CustomLogger):
    async def async_pre_call_deployment_hook(self, kwargs, call_type):
        messages = kwargs.get("messages")
        if (
            kwargs.get("continue_final_message")
            and messages
            and messages[-1].get("role") != "assistant"
        ):
            kwargs["messages"] = [*messages, {"role": "assistant", "content": ""}]
        return kwargs


continuation = FinalMessageContinuation()
