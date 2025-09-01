from flask import Flask

app = Flask(__name__)

@app.route("/")
def hello():
    return "Hello, World on 7860!"

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=7860)  # รันบนทุก interface, port 7860
