import asyncio
import json
import websockets


async def send_request(ws, request):
    print('client out:', json.dumps(request, indent=4))
    await ws.send(json.dumps(request).encode('utf-8'))
    response = await ws.recv()
    response = json.loads(response.decode('utf-8'))
    print('client in:', json.dumps(response, indent=4))
    return response

async def main(uri):
    async with websockets.connect(uri) as ws:
        await send_request(ws, {
                    'type' : 'spectator'
                })

        response = await send_request(ws, {
            'id' : '607afc3082208a39a62f20fe',
            'uri': 'ty loh'
        })

if __name__ == '__main__':
    asyncio.get_event_loop().run_until_complete(main('ws://localhost:8888/streams'))